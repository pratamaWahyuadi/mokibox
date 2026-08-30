# Implementation Plan — TikTok Clone Backend MVP

Dokumen ini adalah **coding plan** untuk implementasi backend TikTok clone. Semua keputusan teknis mengikuti PRD, ERD, dan API Contract yang sudah final. Plan ini cukup detail untuk langsung dieksekusi oleh AI coding agent atau developer manusia.

---

## 0. Asumsi Teknis & Catatan Penting

Beberapa celah teknis yang tidak dieksplisitkan di dokumen sumber saya tutup dengan asumsi paling aman. **Asumsi ini wajib dibaca dulu sebelum implementasi.**

| # | Asumsi | Dampak ke Implementasi |
|---|---|---|
| A1 | **Satu Go module untuk repo**, bukan multi-module. | `go.mod` di root, package `shared` dipakai oleh `api-gateway` dan `transcoder-worker`. Binary di-build via `go build ./api-gateway` dan `go build ./transcoder-worker`. |
| A2 | **sqlc generate output ke `shared/db`**, package name `db`. | File generated (`models.go`, `*.sql.go`) tidak boleh diedit manual. `shared/models.go` tetap dibuat untuk DTO/domain/payload queue. |
| A3 | **S3/R2 presigned PUT tidak mendukung kondisi `content-length-range`.** | Sesuai kontrak, upload tetap `PUT` ke presigned URL. Ukuran file dipaksa divalidasi di `confirm` via `HeadObject` R2. Objek yang ukurannya invalid dihapus via cleanup task. Ini adalah compensating control untuk SEC-08. |
| A4 | **HLS multi-variant 480p + 720p.** | Worker menghasilkan `master.m3u8` + `480p/index.m3u8` + `720p/index.m3u8`. Endpoint `GET /api/videos/:id/playlist.m3u8` mengembalikan master playlist yang sudah di-rewrite; `?variant=480p|720p` untuk variant playlist. URL variant memakai media token. |
| A5 | **Media token HMAC untuk akses playlist**, bukan JWT di query string. | Video response berisi `hls_playlist_url` dengan `?token=<short-lived-hmac>`. Token valid 15 menit, terikat ke `video_id`. Endpoint playlist menerima `Authorization: Bearer` JWT **atau** `?token=`. |
| A6 | **Delete user = tombstone, bukan hard delete `users`.** | `is_active=false`, `deleted_at=NOW()`, `username` diubah placeholder `deleted_<uuid>`, PII di-null. Semua data relasional dihapus. Ini mencegah JWT lama membuat user baru lagi. |
| A7 | **`avatar_url` dianggap URL eksternal / hasil upload di luar scope.** | Tidak ada endpoint upload avatar. API hanya menyimpan dan mengembalikan URL string. |
| A8 | **Worker role PostgreSQL dibatasi.** | Role `tiktok_api` dapat CRUD semua tabel app. Role `tiktok_worker` hanya `SELECT, UPDATE, DELETE` pada `videos`. Tidak ada superuser untuk worker. |
| A9 | **Counter `likes_count`, `comments_count`, `views_count` diupdate di aplikasi dalam transaksi**, bukan trigger. | Konsisten dengan catatan ERD. Untuk delete comment dengan reply, dipakai recursive CTE untuk menghitung seluruh subtree sebelum delete. |
| A10 | **Endpoint `DELETE /api/users/me` wajib ada**, walau tidak ada di tabel endpoint PRD asli, karena API Contract menambahkannya untuk US-09. | Diimplementasikan di Fase 8. |
| A11 | **Zitadel identity provider dideploy sebagai compose project terpisah**, bukan service di dalam `docker-compose.yml` MokiBox. | Sejak Zitadel v3+ arsitektur multi-service (Traefik + zitadel-api + zitadel-login + Postgres sendiri) yang tidak bisa di-merge dengan stack MokiBox tanpa restart loop. MokiBox cuma butuh Zitadel sebagai external dependency (issuer URL + JWKS endpoint). Folder `../zitadel-compose/` (di luar repo MokiBox) memegang stack Zitadel. Makefile punya target `up-zitadel` / `up-all` / `down-zitadel` / `logs-zitadel` untuk manage lifecycle Zitadel terpisah. **Deviasi dari NFR-11 ("satu docker-compose")** — NFR-11 maksudnya "satu VPS" bukan "satu compose file", dan dependency Zitadel sebagai external service lebih sehat secara arsitektur. |

---

## 1. Ringkasan Fase

| Fase | Isi | Estimasi |
|---|---|---|
| Fase 0 | Setup repo, docker-compose, database bootstrap, Zitadel | Hari 1 |
| Fase 1 | Migration SQL, sqlc, shared config, db pool | Hari 2 |
| Fase 2 | Shared utils: response, error, R2 client, Redis/Asynq, media token, cursor | Hari 2–3 |
| Fase 3 | Auth OIDC, user profile, webhook Zitadel | Hari 3 |
| Fase 4 | Upload intent + confirm upload | Hari 4–5 |
| Fase 5 | Transcoder worker: ffprobe, HLS, retry, cleanup | Hari 6–7 |
| Fase 6 | Feed, video detail/status/playlist, follow | Hari 8–9 |
| Fase 7 | Like, comment, reply, view, notifikasi | Hari 10–11 |
| Fase 8 | Delete account, delete video, cleanup R2 | Hari 12 |
| Fase 9 | Wiring API gateway, error mapping, main, router | Hari 13 |
| Fase 10 | Testing, security hardening, demo polish | Hari 14 |

> Urutan bisa disesuaikan, tapi **security requirement tidak boleh dikorbankan**.

---

## 2. Struktur Repo Final

```
tiktok-backend/
├── .env.example
├── .env                     # TIDAK di-commit
├── .gitignore
├── go.mod
├── Makefile
├── docker-compose.yml
│
├── migrations/
│   └── 001_init.sql
│
├── sqlc/
│   ├── sqlc.yaml
│   └── queries.sql
│
├── shared/                  # Dipakai api-gateway & worker
│   ├── config.go
│   ├── db.go
│   ├── redis.go
│   ├── r2.go
│   ├── models.go
│   ├── errors.go
│   ├── response.go
│   ├── cursor.go
│   ├── mediatoken.go
│   └── db/                  # Generated sqlc, jangan diedit
│       ├── models.go
│       └── *.sql.go
│
├── api-gateway/
│   ├── main.go
│   ├── routes.go
│   ├── handlers/
│   │   ├── user.go
│   │   ├── video.go
│   │   ├── feed.go
│   │   ├── social.go
│   │   ├── notification.go
│   │   ├── account.go
│   │   └── webhook.go
│   ├── middleware/
│   │   ├── auth.go
│   │   └── ratelimit.go
│   ├── Dockerfile
│   └── ...
│
├── transcoder-worker/
│   ├── main.go
│   ├── ffprobe.go
│   ├── transcode.go
│   ├── cleanup.go
│   ├── Dockerfile
│   └── ...
│
├── deploy/
│   ├── nginx/default.conf
│   └── postgres/init/         # Script create roles
│
└── scripts/
    ├── bootstrap_db.sh
    └── smoke_test.sh
```

---

## 3. Fase 0 — Setup Repo, Infra, Docker Compose, Zitadel

> **Asumsi A11 update (post-fase-3)**: Zitadel v3+ arsitektur multi-service
> (Traefik + zitadel-api + zitadel-login + Postgres sendiri) yang tidak
> fit di dalam satu `docker-compose.yml` MokiBox. Zitadel sekarang
> dideploy sebagai compose project terpisah di `../zitadel-compose/`
> (di luar repo MokiBox). MokiBox cuma refer Zitadel sebagai external
> dependency via `ZITADEL_ISSUER_URL`. Makefile punya target
> `up-zitadel` / `up-all` / `down-zitadel` / `logs-zitadel` untuk
> manage lifecycle-nya. Detail di Asumsi A11.

### Tujuan
Semua service app MokiBox jalan di satu VPS via `docker-compose`, dan
PostgreSQL/Redis tidak terpapar ke host. Zitadel dideploy terpisah
(lihat catatan A11 di atas) sebagai identity-provider stack independen
yang share cuma network host.

### Task

1. **Inisialisasi repo dan Go module**
   ```
   git init
   go mod init github.com/<org>/tiktok-backend
   ```
   Tambahkan `.gitignore`: `.env`, `bin/`, `*.log`, `data/`.

2. **Buat `.env.example`** berisi placeholder:
   ```
   POSTGRES_USER=postgres
   POSTGRES_PASSWORD=change-me
   POSTGRES_DB=tiktok

   TIKTOK_API_DB_PASSWORD=change-me
   TIKTOK_WORKER_DB_PASSWORD=change-me

   REDIS_PASSWORD=change-me
   REDIS_ADDR=redis:6379

   ZITADEL_ISSUER_URL=https://auth.example.com
   ZITADEL_CLIENT_ID=xxx
   ZITADEL_API_CLIENT_ID=xxx
   ZITADEL_TARGET_SIGNING_KEY=xxx

   R2_ACCOUNT_ID=xxx
   R2_ACCESS_KEY_ID=xxx
   R2_SECRET_ACCESS_KEY=xxx
   R2_BUCKET=tiktok-media

   API_BASE_URL=https://api.example.com
   MEDIA_TOKEN_SECRET=xxx
   MEDIA_TOKEN_TTL=15m

   DATABASE_URL=postgres://tiktok_api:change-me@postgres:5432/tiktok?sslmode=disable
   WORKER_DATABASE_URL=postgres://tiktok_worker:change-me@postgres:5432/tiktok?sslmode=disable

   PRESIGN_UPLOAD_EXPIRY=15m
   TRANSCODE_TIMEOUT=5m
   ```

3. **Buat `docker-compose.yml`**
   - Service: `postgres:16-alpine`, `redis:7-alpine`, `api-gateway`, `transcoder-worker`, `nginx` (Zitadel **bukan** service di sini, lihat A11).
   - **PostgreSQL tidak publish port ke host.**
   - **Redis tidak publish port ke host**, jalankan dengan `--requirepass ${REDIS_PASSWORD}`.
   - Volume untuk `pgdata` dan `redisdata`.
   - Network internal bridge.
   - Nginx publish port `80` dan `443`.

   Poin penting:
   ```yaml
   postgres:
     image: postgres:16-alpine
     environment:
       POSTGRES_USER: ${POSTGRES_USER}
       POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
       POSTGRES_DB: ${POSTGRES_DB}
     volumes:
       - pgdata:/var/lib/postgresql/data
       - ./deploy/postgres/init:/docker-entrypoint-initdb.d:ro

   redis:
     image: redis:7-alpine
     command: ["redis-server", "--requirepass", "${REDIS_PASSWORD}", "--appendonly", "yes"]

   transcoder-worker:
     build: ./transcoder-worker
     read_only: true
     tmpfs:
       - /tmp
     cap_drop:
       - ALL
     security_opt:
       - no-new-privileges:true
     user: "10001:10001"
     pids_limit: 200
     environment:
       WORKER_DATABASE_URL: ${WORKER_DATABASE_URL}
       REDIS_ADDR: ${REDIS_ADDR}
       REDIS_PASSWORD: ${REDIS_PASSWORD}
       ...
   ```

4. **Buat `deploy/postgres/init/01_app_roles.sql`** untuk create role API dan worker:
   ```sql
   CREATE ROLE tiktok_api LOGIN PASSWORD 'change-me';
   CREATE ROLE tiktok_worker LOGIN PASSWORD 'change-me';
   ```
   > Password role benar-benar diisi dari environment. Untuk produksi, lebih aman dibuat manual via `scripts/bootstrap_db.sh`, bukan di-commit dengan password asli. Docker init SQL hanya digunakan untuk development lokal.
   > File ini TIDAK bootstrap Zitadel — Zitadel punya Postgres sendiri (lihat A11).

5. **Buat `deploy/nginx/default.conf`**
   - `server` untuk `api.example.com`.
   - `location /api/` → `proxy_pass http://api-gateway:8080;`
   - ~~`location /zitadel/` → `proxy_pass http://zitadel:8080;` + WebSocket headers.~~
     Dihapus per A11: Zitadel di-reverse-proxy oleh Traefik di
     `zitadel-compose` sendiri, bukan Nginx MokiBox. CORS / auth flow
     di-handle di sisi Zitadel, MokiBox cuma konsumsi JWT via JWKS.
   - `location /healthz` → `proxy_pass http://api-gateway:8080/healthz;`
   - Aktifkan HSTS, TLS 1.2+, `client_max_body_size 1m` (upload tidak lewat Nginx, langsung ke R2).

6. **Buat `Makefile`**
   - `make db-up` → start postgres+redis.
   - `make db-bootstrap` → jalankan script create roles.
   - `make db-migrate` → `psql -d tiktok -f migrations/001_init.sql`.
   - `make sqlc-gen` → jalankan sqlc.
   - `make build` → build kedua binary.
   - `make up` → `docker compose up -d --build`.
   - `make test` → `go test ./...`.

7. **Setup Zitadel**
   - Jalankan compose setelah Dockerfile placeholder dibuat, atau langsung pakai image Zitadel saja dulu.
   - Buka `https://auth.example.com`, selesaikan setup instance pertama.
   - Buat **Organization**, **Project**, **Application** tipe Web/OIDC untuk login (client ID → `ZITADEL_CLIENT_ID`).
   - Buat **Application** kedua bertipe **API** di project yang sama untuk resource server (client ID → `ZITADEL_API_CLIENT_ID`).
     - Di Token Settings application ini, set **Auth Token Type = JWT** (default-nya opaque), supaya access token bisa divalidasi lokal via JWKS tanpa panggil endpoint introspection.
   - Catat kedua Client ID dan issuer URL.
   - Aktifkan Google Sign-In di Zitadel Console.
   - Setup **Actions V2** untuk notifikasi lifecycle user (menggantikan "Webhook" generik yang tidak tersedia di Zitadel):
     1. `CreateTarget` → endpoint `https://api.example.com/api/webhooks/zitadel`. Simpan `signingKey` dari response sebagai `ZITADEL_TARGET_SIGNING_KEY`.
     2. `SetExecution` dua kali: satu dengan `condition.event.event = "user.removed"`, satu lagi dengan `condition.event.event = "user.deactivated"`, keduanya arahkan ke target ID dari langkah 1.
   - Pastikan `ZITADEL_ISSUER_URL` di `.env` adalah URL publik yang sama dengan issuer di console.

### Best Practice Fase 0
- Semua password hanya di `.env`, tidak di-commit.
- Pin semua image version, jangan `latest`.
- PostgreSQL/Redis hanya bisa diakses dari Docker network.
- Setup Zitadel di hari pertama, karena paling berisiko.

---

## 4. Fase 1 — Database Migration & sqlc

### Tujuan
Skema database final + generated Go code dari sqlc.

### Task

1. **Tulis `migrations/001_init.sql`**
   - Copy **verbatim** DDL dari dokumen ERD.
   - Tambahkan di akhir:
     ```sql
     GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO tiktok_api;
     GRANT SELECT, UPDATE, DELETE ON videos TO tiktok_worker;
     ```
   - Migration bersifat **forward-only**. Tidak boleh diubah setelah dijalankan. Perubahan berikutnya = `002_*.sql`.

2. **Buat `sqlc/sqlc.yaml`**
   ```yaml
   version: "2"
   sql:
     - engine: "postgresql"
       schema: "../migrations/001_init.sql"
       queries: "queries.sql"
       gen:
         go:
           package: "db"
           out: "../shared/db"
           emit_json_tags: true
           emit_empty_slices: true
           overrides:
             - db_type: "uuid"
               go_type:
                 import: "github.com/google/uuid"
                 package: "uuid"
                 type: "UUID"
             - db_type: "timestamptz"
               go_type:
                 import: "time"
                 package: "time"
                 type: "Time"
   ```

3. **Tulis `sqlc/queries.sql`** — berisi query yang dibutuhkan semua handler. Minimal query berikut:

   **Users**
   - `GetUserByZitadelID` :one
   - `CreateUser` :one — `INSERT ... ON CONFLICT (zitadel_id) DO NOTHING RETURNING *`
   - `GetUserByID` :one
   - `UpdateUserProfile` :one — pakai `COALESCE(sqlc.narg(...), col)` agar field opsional
   - `DeactivateUser` :one — set `is_active=false`
   - `TombstoneUser` :one — set `is_active=false`, `deleted_at=NOW()`, `username='deleted_'||id::text`, PII null
   - `GetUserProfileWithStats` :one — query `users` + `follower_count`, `following_count`, `is_following`

   **Videos**
   - `InsertVideo` :one — insert `PENDING_UPLOAD`
   - `GetPendingVideoByUser` :one
   - `UpdatePendingVideoR2Key` :one
   - `GetVideoByID` :one
   - `GetVideoByIDForUpdate` :one — `SELECT ... FOR UPDATE`
   - `ConfirmVideoProcessing` :one — `UPDATE videos SET status='PROCESSING' WHERE id=$1 AND user_id=$2 AND status='PENDING_UPLOAD' AND r2_key=$3 RETURNING *`
   - `GetVideoDetail` :one — join `users`, plus `EXISTS(...) AS liked_by_me`
   - `GetVideoStatusByID` :one
   - `ListVideosByUser` :many — user sendiri semua status kecuali `DELETED`; user lain hanya `READY`
   - `ListFeedVideos` :many — `READY`, exclude self, user publik + following, cursor pagination
   - `MarkVideoDeleted` :one — `status='DELETED', deleted_at=NOW()`
   - `MarkVideoFailed` :one
   - `MarkVideoReady` :one
   - `IncrementVideoRetry` :one
   - `UpdateVideoToProcessing` :one — untuk retry task
   - `IncrementViews` :one
   - `IncrementLikesCount` :one
   - `DecrementLikesCount` :one
   - `IncrementCommentsCount` :one
   - `DecrementCommentsCount` :one
   - `DecrementLikesForUser` :exec — update `likes_count` untuk semua video yang di-like user yang dihapus
   - `DecrementCommentsForUser` :exec — update `comments_count` untuk semua video yang dikomentari user yang dihapus
   - `ListVideoKeysByUser` :many — ambil `r2_key`, `hls_prefix`, `thumbnail_key` untuk cleanup
   - `DeleteVideosByUser` :exec
   - `ListVideosEligibleForCleanup` :many — `status='DELETED' AND deleted_at < NOW() - INTERVAL '24 hours'`
   - `DeleteVideoRow` :exec

   **Follows**
   - `FollowUser` :exec — `INSERT ... ON CONFLICT DO NOTHING`
   - `DeleteFollow` :exec
   - `IsFollowing` :one
   - `ListFollowers` :many — join `users`
   - `ListFollowing` :many
   - `CountFollowers` :one
   - `CountFollowing` :one
   - `DeleteFollowsByFollower` :exec
   - `DeleteFollowsByFollowee` :exec

   **Likes**
   - `InsertLike` :one — `ON CONFLICT DO NOTHING RETURNING *`
   - `DeleteLike` :one — `DELETE ... RETURNING *`
   - `IsLiked` :one
   - `DeleteLikesByUser` :exec

   **Comments**
   - `InsertComment` :one
   - `GetCommentByID` :one
   - `ListCommentsByVideo` :many — join `users`, cursor pagination
   - `CountCommentSubtree` :one — recursive CTE untuk total comment yang akan terhapus termasuk reply
   - `DeleteCommentByID` :exec
   - `DeleteCommentsByUser` :exec

   **Notifications**
   - `InsertNotification` :one
   - `ListNotifications` :many — cursor pagination
   - `MarkAllNotificationsRead` :execrows
   - `DeleteNotificationsForUser` :exec
   - `DeleteNotificationsByActor` :exec

4. **Generate sqlc**
   ```
   make sqlc-gen
   ```
   Hasil: `shared/db/*.go`.

5. **Buat `shared/config.go`**
   - Struct `Config` berisi semua env di atas.
   - Function `Load()` membaca `os.Getenv`.
   - Pisahkan `APIConfig` dan `WorkerConfig`.

6. **Buat `shared/db.go`**
   - `func NewDB(ctx context.Context, databaseURL string) (*pgxpool.Pool, error)`
   - Set pool config: max conns API = 10, worker = 5.

### Best Practice Fase 1
- Jangan pernah menulis struct model manual untuk tabel; andalkan sqlc.
- Setiap query yang butuh atomicity pakai `FOR UPDATE` / transaction.
- `queries.sql` adalah satu-satunya sumber query SQL. Tidak ada raw SQL di handler.

---

## 5. Fase 2 — Shared Utils & External Clients

### Tujuan
Semua komponen lintas service siap: response format, error, R2, Redis/Asynq, media token, cursor.

### Task

1. **Buat `shared/errors.go`**
   - Define sentinel error dan mapping ke HTTP status.
   - Kode error mengikuti API Contract:
     - `VALIDATION_ERROR` → 400
     - `UNAUTHORIZED` → 401
     - `FORBIDDEN` → 403
     - `NOT_FOUND` → 404
     - `VIDEO_STATUS_CONFLICT` → 409
     - `VIDEO_NOT_READY` → 409
     - `UPLOAD_MISSING` → 409
     - `UPLOAD_SIZE_INVALID` → 400
     - `SELF_FOLLOW_NOT_ALLOWED` → 400
     - `WEBHOOK_INVALID_SIGNATURE` → 401
     - `WEBHOOK_EVENT_UNSUPPORTED` → 400
     - `INTERNAL_ERROR` → 500

2. **Buat `shared/response.go`**
   - `RespondOK(c, data)`
   - `RespondCreated(c, data)`
   - `RespondNoContent(c)`
   - `RespondError(c, err)`
   - Envelope: `{"data": ...}`, list: `{"data": [], "pagination": {"next_cursor": null}}`, error: `{"error": {"code", "message", "details"}}`.

3. **Buat `shared/cursor.go`**
   - Format cursor: `base64url("<RFC3339Nano>|<uuid>")`.
   - Function `EncodeCursor(createdAt time.Time, id uuid.UUID) string`
   - Function `DecodeCursor(cursor string) (time.Time, uuid.UUID, error)`
   - Query pakai `(created_at, id) < ($created, $id)`.

4. **Buat `shared/mediatoken.go`**
   - `GenerateMediaToken(videoID string, secret string, ttl time.Duration) (string, error)`
   - Payload: `video_id:<expiry_unix>` + HMAC-SHA256.
   - `VerifyMediaToken(token string, videoID string, secret string) error`.
   - Gunakan `hmac.Equal` untuk comparison constant-time.

5. **Buat `shared/r2.go`**
   - Wrapper `R2Client` menggunakan `aws-sdk-go-v2`.
   - Method:
     - `PresignPut(ctx, key, contentType, expiry) (string, error)` — presigned PUT.
     - `HeadObject(ctx, key) (size int64, err error)` — untuk validasi ukuran di confirm.
     - `PresignGet(ctx, key, expiry) (string, error)` — thumbnail dan segment HLS.
     - `DeleteObjects(ctx, keys []string) error`
     - `Download(ctx, key, destPath) error`
     - `UploadFile(ctx, key, filePath, contentType) error`
   - Endpoint R2:
     ```
     https://<ACCOUNT_ID>.r2.cloudflarestorage.com
     ```

6. **Buat `shared/redis.go` + `shared/models.go`**
   - Inisialisasi Asynq client dan server.
   - Task type constants:
     ```go
     const (
       TypeTranscodeVideo = "transcode:video"
       TypeCleanupObjects = "cleanup:objects"
       TypeCleanupVideo   = "cleanup:video"
     )
     ```
   - Payload structs:
     ```go
     type TranscodeVideoPayload struct {
       VideoID string `json:"video_id"`
     }

     type CleanupObjectsPayload struct {
       Keys []string `json:"keys"`
     }

     type CleanupVideoPayload struct {
       VideoID string `json:"video_id"`
     }
     ```

### Best Practice Fase 2
- `R2Client` dan Asynq client di-inject via constructor, bukan global variable.
- Semua operasi eksternal lewat interface/abstraksi agar mudah di-mock saat testing.
- `MEDIA_TOKEN_SECRET` dan `ZITADEL_TARGET_SIGNING_KEY` hanya ada di env.

---

## 6. Fase 3 — Auth OIDC, User Profile, Webhook Zitadel

### Tujuan
Semua endpoint `/api/*` kecuali webhook bisa mengenali user dari JWT Zitadel. User profile CRUD dan webhook user deleted/deactivated jalan.

### Task

1. **Buat `api-gateway/middleware/auth.go`**
   - Pakai SDK resmi `github.com/zitadel/zitadel-go/v3` (`pkg/authorization` + `pkg/authorization/oauth`), **bukan** `coreos/go-oidc`.
     `go-oidc` `provider.Verifier()` memverifikasi **ID Token**, bukan **access token** yang dikirim client — dua hal beda secara struktur klaim. Untuk resource API, dipakai `oauth.DefaultJWTAuthorization`, metode yang direkomendasikan Zitadel: validasi access token JWT secara lokal via JWKS, tanpa call introspection endpoint dan tanpa perlu simpan sign key sendiri.
   - Inisialisasi sekali saat startup (mis. di `main.go`, di-inject ke middleware):
     ```go
     authZ, err := authorization.New(
         ctx,
         zitadel.New(cfg.ZitadelIssuerURL),
         oauth.DefaultJWTAuthorization(cfg.ZitadelAPIClientID),
     )
     ```
     `DefaultJWTAuthorization` melakukan OIDC Discovery untuk menemukan `jwks_uri`, lalu bikin `rp.RemoteKeySet` yang fetch + cache public key di memory dan otomatis refresh saat key di-rotate oleh Zitadel.
   - Middleware `Authenticate`:
     1. Ambil token dari `Authorization: Bearer <token>`.
     2. `authCtx, err := authZ.CheckAuthorization(ctx, rawToken)` → validasi signature (via JWKS), issuer, audience (`cfg.ZitadelAPIClientID`), expiry, algoritma sekaligus.
     3. Ambil `sub` dari `authCtx.UserID()`.
     4. Panggil `GetOrCreateUser(ctx, db, sub)`.
     5. Jika `is_active=false` → return `401 UNAUTHORIZED`.
     6. Simpan `*db.User` di Echo context.
   - `GetOrCreateUser`: (tidak berubah)
     - `GetUserByZitadelID(sub)`.
     - Jika `ErrNoRows` → generate username `user_<random12hex>`, `CreateUser`.
     - Jika `CreateUser` kena unique violation karena race → re-select.
   - Untuk testability, definisikan interface kecil `TokenVerifier` (wrap `authorization.Verifier[*oauth.IntrospectionContext]`) di middleware dan real implementation dibungkus.

2. **Buat `api-gateway/handlers/user.go`**
   - Handler struct:
     ```go
     type UserHandler struct {
       DB *pgxpool.Pool
       Queries *db.Queries
       R2 *shared.R2Client
       Queue *asynq.Client
       Cfg shared.Config
     }
     ```
   - `GetMe`: return `UserProfile`.
   - `UpdateMe`:
     - Body: `display_name`, `bio`, `avatar_url`, `is_private` — semua opsional.
     - `username` immutable.
     - Pakai `UpdateUserProfile` dengan `sqlc.narg`.
   - `GetUserProfile`:
     - `GetUserProfileWithStats`.
     - Jika user tidak ada / `is_active=false` → `404 NOT_FOUND`.
     - Return `UserProfile` + `is_following`, `follower_count`, `following_count`.
   - `GetUserVideos`:
     - Cek user target aktif.
     - Jika user target private dan requester bukan follower → `404`.
     - Owner boleh lihat semua status kecuali `DELETED`.
     - Non-owner hanya `READY`.
     - Return list `VideoObject` + pagination.

3. **Buat `api-gateway/handlers/webhook.go`**
   - Endpoint `POST /api/webhooks/zitadel` menerima panggilan dari **Actions V2 Target** yang dibuat di Fase 0 (bukan "Webhook" generik — fitur itu tidak ada di Zitadel).
   - Middleware terpisah: `RateLimitWebhook`.
   - Verifikasi signature pakai helper resmi `github.com/zitadel/zitadel-go/v3/pkg/actions`:
     ```go
     if err := actions.ValidateRequestPayload(rawBody, &req.Header, cfg.ZitadelTargetSigningKey); err != nil {
       return shared.ErrWebhookInvalidSignature
     }
     ```
     Header resminya `ZITADEL-Signature`, isi `t=<timestamp>,v1=<hmac_hex>`; HMAC dihitung Zitadel dari `timestamp + "." + raw_body` menggunakan `signingKey` yang didapat sewaktu `CreateTarget`. Jangan re-implement HMAC manual — pakai helper `actions.ValidateRequestPayload` supaya cara hitungnya selalu konsisten dengan Zitadel.
   - Parse body sesuai payload event Actions V2 (bukan envelope custom):
     ```json
     {
       "aggregateID": "336494809936035843",
       "aggregateType": "user",
       "resourceOwner": "336392597046099971",
       "instanceID": "336392597046034435",
       "version": "v2",
       "sequence": 1,
       "event_type": "user.removed",
       "created_at": "2025-01-01T12:20:00Z",
       "userID": "336392597046755331",
       "event_payload": { }
     }
     ```
     Ambil user ID terdampak dari field **`userID`** (bukan field custom `user_id`).
   - `event_type == "user.deactivated"` → `DeactivateUser(userID)`.
   - `event_type == "user.removed"` → panggil `DeleteUserData(userID)` (implementasi di Fase 8).
   - Event lain yang ke-trigger tak sengaja → `400 WEBHOOK_EVENT_UNSUPPORTED`.
   - Log raw body + `event_type` ke stdout untuk audit sederhana.

4. **Buat `api-gateway/routes.go`**
   ```go
   e.GET("/healthz", health)
   e.POST("/api/webhooks/zitadel", webhookHandler.Handle, middleware.RateLimitWebhook)

   api := e.Group("/api", middleware.Authenticate)
   api.GET("/users/me", userHandler.GetMe)
   api.PUT("/users/me", userHandler.UpdateMe)
   api.DELETE("/users/me", userHandler.DeleteMe)
   api.GET("/users/:id", userHandler.GetUserProfile)
   api.GET("/users/:id/videos", userHandler.GetUserVideos)
   ```

### Best Practice Fase 3
- Jangan pernah memvalidasi JWT manual. `zitadel-go` (`oauth.DefaultJWTAuthorization`) sudah menangani signature (via JWKS, auto-refresh saat rotasi key), issuer, audience, expiry, dan alg — tanpa perlu simpan sign key sendiri di server.
- Jangan simpan email.
- Endpoint read yang tidak berhak → `404`, bukan `403`, untuk mencegah enumerasi.
- Verifikasi signature Actions V2 pakai helper resmi `actions.ValidateRequestPayload` (constant-time di dalamnya), bukan implementasi HMAC manual.

---

## 7. Fase 4 — Upload Intent & Confirm

### Tujuan
User bisa minta presigned URL upload ke R2, upload langsung ke R2, lalu konfirmasi dan masuk antrian transcode.

### Task

1. **Implement `api-gateway/handlers/video.go`**

   **`POST /api/videos/upload-intent`**
   - Body: `title`, `description` opsional.
   - Cari `GetPendingVideoByUser(userID)`:
     - Jika tidak ada:
       - Generate `videoID = uuid.New()`.
       - `r2Key = "uploads/<userID>/<videoID>/source.mp4"`.
       - `InsertVideo` dengan status `PENDING_UPLOAD`.
       - Return `201 Created`.
     - Jika ada:
       - `oldKey = existing.R2Key`.
       - Generate `newKey = "uploads/<userID>/<existing.ID>/source.mp4"`.
       - `UpdatePendingVideoR2Key` + update title/description.
       - Jika `oldKey != newKey`, enqueue `cleanup:objects` untuk `oldKey` (best effort).
       - Return `200 OK`.
   - Generate presigned PUT:
     - `PresignPut(ctx, r2Key, "application/octet-stream", expiry=15m)`.
   - Response sesuai API Contract:
     ```json
     {
       "data": {
         "video_id": "...",
         "r2_key": "...",
         "http_method": "PUT",
         "upload_url": "https://...",
         "upload_headers": { "Content-Type": "application/octet-stream" },
         "min_size_bytes": 1024,
         "max_size_bytes": 209715200,
         "expires_at": "..."
       }
     }
     ```

   **`POST /api/videos/confirm`**
   - Body: `video_id`, `r2_key`.
   - `GetVideoByIDForUpdate(videoID)`.
   - Validasi:
     - Video ada → else `404`.
     - `video.UserID == currentUser.ID` → else `404`.
     - `status == "PENDING_UPLOAD"` → else `409 VIDEO_STATUS_CONFLICT`.
     - `video.R2Key == body.r2_key` → else `400 VALIDATION_ERROR`.
   - `HeadObject` R2:
     - Object tidak ada → `409 UPLOAD_MISSING`.
     - Size < 1024 atau > 209_715_200 → `400 UPLOAD_SIZE_INVALID`, lalu enqueue cleanup untuk hapus object invalid.
   - `ConfirmVideoProcessing` → status `PROCESSING`, `retry_count=0`.
   - Enqueue task `transcode:video`:
     ```go
     client.Enqueue(task, asynq.MaxRetry(1))
     ```
   - Jika enqueue gagal → `UpdateVideoToPending` kembali, return `500`.
   - Response `200 OK`:
     ```json
     {
       "data": {
         "video_id": "...",
         "status": "PROCESSING",
         "retry_count": 0
       }
     }
     ```

2. **Tambahkan routes**
   ```go
   api.POST("/videos/upload-intent", videoHandler.UploadIntent)
   api.POST("/videos/confirm", videoHandler.ConfirmUpload)
   ```

### Best Practice Fase 4
- `FOR UPDATE` mencegah double confirm.
- Jangan percaya body/klaim ukuran; selalu cek `HeadObject` R2.
- Record `PENDING_UPLOAD` per user dijamin unik oleh partial unique index database.

---

## 8. Fase 5 — Transcoder Worker

### Tujuan
Worker memproses video dengan aman: validasi ffprobe, transcode HLS 480p/720p, thumbnail, retry max 3, cleanup R2.

### Task

1. **Buat `transcoder-worker/main.go`**
   - Load `WorkerConfig`.
   - Connect DB, R2, Redis.
   - Setup Asynq server:
     ```go
     srv := asynq.NewServer(
       asynq.RedisClientOpt{Addr: cfg.RedisAddr, Password: cfg.RedisPassword},
       asynq.Config{
         Concurrency: 1,
         Queues: map[string]int{"default": 10},
         Logger: asynq.GetLogger(),
       },
     )
     mux := asynq.NewServeMux()
     mux.HandleFunc(shared.TypeTranscodeVideo, w.HandleTranscode)
     mux.HandleFunc(shared.TypeCleanupObjects, w.HandleCleanupObjects)
     mux.HandleFunc(shared.TypeCleanupVideo, w.HandleCleanupVideo)
     srv.Run(mux)
     ```

2. **Buat `transcoder-worker/ffprobe.go`**
   - `ProbeFile(filePath)` execute `ffprobe -v error -print_format json -show_format -show_streams`.
   - `ValidateMedia(probe)`:
     - Wajib ada minimal 1 video stream.
     - `codec_name` video masuk allowlist: `h264`, `hevc`, `vp9`, `av1`.
     - Audio opsional; jika ada, codec allowlist: `aac`, `opus`, `mp3`.
     - `duration_seconds` antara 1 dan 180.
     - `width`/`height` antara 16 dan 4096.
     - `bit_rate` (jika ada) ≤ 25_000_000.
     - `format.size` (dari R2 head object) 1 KB – 200 MB.
   - Jika invalid → return error deskriptif; worker set `FAILED`.

3. **Buat `transcoder-worker/transcode.go`**

   **Alur `HandleTranscode`:**
   1. Parse payload `video_id`.
   2. `GetVideoByID`.
      - Video tidak ditemukan → `SkipRetry` (user sudah dihapus).
      - `status != "PROCESSING"` → `SkipRetry` (misal sudah `DELETED`/`READY`/`FAILED`).
      - `retry_count >= 3` → set `FAILED`, return nil.
   3. Increment `retry_count`:
      ```sql
      UPDATE videos SET retry_count = retry_count + 1, status = 'PROCESSING' WHERE id = $1;
      ```
   4. Buat temp dir di `/tmp/transcode/<videoID>-<random>` (tmpfs).
   5. Download raw dari R2 ke `source.bin`.
   6. Jalankan `ffprobe` + `ValidateMedia`.
      - Invalid → `MarkVideoFailed`, hapus raw di R2, cleanup temp, return nil (tidak retry).
   7. Jalankan FFmpeg:
      - 480p:
        ```
        ffmpeg -y -i source.bin \
          -vf scale=-2:480 -c:v libx264 -preset veryfast -crf 23 \
          -c:a aac -b:a 128k \
          -hls_time 6 -hls_playlist_type vod \
          -hls_segment_filename <hls_prefix>/480p/segment_%04d.ts \
          <hls_prefix>/480p/index.m3u8
        ```
      - 720p:
        ```
        ffmpeg -y -i source.bin \
          -vf scale=-2:720 -c:v libx264 -preset veryfast -crf 23 \
          -c:a aac -b:a 128k \
          -hls_time 6 -hls_playlist_type vod \
          -hls_segment_filename <hls_prefix>/720p/segment_%04d.ts \
          <hls_prefix>/720p/index.m3u8
        ```
      - Thumbnail:
        ```
        ffmpeg -y -i source.bin -ss 00:00:01 -frames:v 1 -vf scale=480:-1 <thumbnail_key>
        ```
      - `hls_prefix = "hls/<userID>/<videoID>"`.
      - `thumbnail_key = "thumbs/<userID>/<videoID>/thumb.jpg"`.
   8. Upload semua file HLS + thumbnail ke R2.
   9. Buat `master.m3u8` yang me-reference `480p/index.m3u8` dan `720p/index.m3u8`, upload juga.
   10. **Sebelum update DB**, cek ulang status video:
       - Jika `status != "PROCESSING"` atau video sudah tidak ada → hapus hasil HLS/thumbnail yang baru diupload dari R2, cleanup temp, return nil.
       - Jika `status = "DELETED"` → jangan publish.
   11. Update DB:
       ```sql
       UPDATE videos
       SET status = 'READY',
           duration_seconds = <from ffprobe>,
           hls_prefix = '<hls_prefix>',
           thumbnail_key = '<thumbnail_key>'
       WHERE id = $1 AND status = 'PROCESSING';
       ```
   12. Hapus raw object dari R2.
   13. Cleanup temp dir.

   **Retry logic:**
   - Jika error transcode transient:
     - Jika `retry_count` sekarang < 3 → enqueue ulang task `transcode:video` dengan `ProcessIn(30s * retry_count)`.
     - Jika sudah ≥ 3 → `MarkVideoFailed`, enqueue cleanup raw, return nil.
   - Gunakan context timeout `TRANSCODE_TIMEOUT=5m`. Jika timeout, kill process, treat sebagai transient.

4. **Buat `transcoder-worker/cleanup.go`**
   - `HandleCleanupObjects`: terima `CleanupObjectsPayload{Keys}` → `R2.DeleteObjects`.
   - `HandleCleanupVideo`: terima `CleanupVideoPayload{VideoID}`:
     - Load video.
     - Jika `status == "DELETED"` dan `deleted_at <= now - 24h` → collect `r2_key`, `hls_prefix`, `thumbnail_key`, delete dari R2, `DeleteVideoRow`.
     - Jika `deleted_at` belum 24 jam → re-enqueue dengan sisa delay.
     - Jika video sudah tidak ada → skip.

5. **Buat `transcoder-worker/Dockerfile`**
   - Build stage: `golang:1.22-alpine`.
   - Runtime: base image dengan FFmpeg terbaru, misal `ubuntu:22.04` + install `ffmpeg`.
   - Buat user non-root `app` UID 10001.
   - Copy binary, set `USER app`.
   - Entrypoint: `./transcoder-worker`.

### Best Practice Fase 5
- Worker **tidak menerima HTTP request**. Hanya dari Redis queue.
- File divalidasi dengan `ffprobe` **sebelum** FFmpeg.
- Jangan percaya ukuran dari request; pakai size dari R2 HeadObject.
- Worker non-root, read-only FS, cap drop, pids/memory limit.
- Cek status video sebelum update DB untuk mencegah publish video yang sudah di-delete.

---

## 9. Fase 6 — Feed, Video Detail/Status/Playlist, Follow

### Tujuan
Home feed, akses video detail, HLS private, dan follow/unfollow.

### Task

1. **Buat `api-gateway/handlers/feed.go`**
   - `GET /api/feed/home`:
     - Ambil `limit` (default 20, max 50) dan `cursor`.
     - Query `ListFeedVideos`:
       ```sql
       WHERE v.status = 'READY'
         AND u.is_active = true
         AND v.user_id <> $viewer_id
         AND (u.is_private = false
              OR EXISTS (SELECT 1 FROM follows f
                         WHERE f.follower_id = $viewer_id
                           AND f.followee_id = v.user_id))
       ORDER BY v.created_at DESC, v.id DESC
       ```
     - Untuk setiap video, buat `VideoObject`:
       - `thumbnail_url` = `R2.PresignGet(thumbnail_key)` jika `READY`, selain itu `null`.
       - `hls_playlist_url` = `API_BASE_URL/api/videos/<id>/playlist.m3u8?token=<mediaToken>` jika `READY`.
       - `liked_by_me` dari query.
       - `is_owner = false`.
     - Return envelope list + pagination.

2. **Lengkapi `api-gateway/handlers/video.go`**

   **`GET /api/videos/:id`**
   - `GetVideoDetail`.
   - Visibility check:
     - Owner → boleh semua status.
     - Non-owner:
       - user owner tidak aktif → `404`.
       - status != `READY` → `404`.
       - owner private dan bukan follower → `404`.
   - Return `VideoObject`.

   **`GET /api/videos/:id/status`**
   - Hanya owner.
   - Non-owner → `404`.
   - Return `{id, status, retry_count, duration_seconds}`.

   **`GET /api/videos/:id/playlist.m3u8`**
   - Auth:
     - Terima `Authorization: Bearer` → validasi JWT seperti biasa, lalu cek akses video.
     - Atau terima `?token=` → validasi media token.
   - Jika `status != READY` → `404`.
   - Param `?variant=480p|720p`:
     - Tanpa variant → ambil `hls_prefix/master.m3u8`, rewrite URL variant menjadi endpoint API dengan token baru.
     - Dengan variant → ambil `hls_prefix/<variant>/index.m3u8`, rewrite setiap baris `.ts` menjadi presigned R2 URL.
   - Response header: `Content-Type: application/vnd.apple.mpegurl`.

   **`DELETE /api/videos/:id`** (P1)
   - Owner only.
   - `MarkVideoDeleted` → status `DELETED`, `deleted_at=NOW()`.
   - Enqueue task `cleanup:video` dengan `ProcessIn(24h)`.
   - Response `204`.

3. **Buat `api-gateway/handlers/users_follow.go`** (bisa digabung di `user.go`)
   - `POST /api/users/:id/follow`
     - Target harus ada & aktif.
     - `id == current user` → `400 SELF_FOLLOW_NOT_ALLOWED`.
     - `FollowUser` idempotent.
     - Insert notification ke target: `type=follow`, `actor=current user`, payload `{"username": ...}`.
     - Response sesuai contract.
   - `DELETE /api/users/:id/follow`
     - Idempotent.
     - `DeleteFollow`.
   - `GET /api/users/:id/followers` → `ListFollowers` + cursor.
   - `GET /api/users/:id/following` → `ListFollowing` + cursor.

4. **Routes tambahan**
   ```go
   api.GET("/feed/home", feedHandler.HomeFeed)
   api.GET("/videos/:id", videoHandler.GetVideoDetail)
   api.GET("/videos/:id/status", videoHandler.GetVideoStatus)
   api.GET("/videos/:id/playlist.m3u8", videoHandler.GetPlaylist)
   api.DELETE("/videos/:id", videoHandler.DeleteVideo)

   api.POST("/users/:id/follow", userHandler.FollowUser)
   api.DELETE("/users/:id/follow", userHandler.UnfollowUser)
   api.GET("/users/:id/followers", userHandler.ListFollowers)
   api.GET("/users/:id/following", userHandler.ListFollowing)
   ```

### Best Practice Fase 6
- UUID bukan security. Semua akses dicek di handler.
- `playlist.m3u8` harus rewrite URL segment jadi presigned URL; jangan pernah expose URL statis R2.
- Private account → non-follower dapat `404`, bukan `403`.

---

## 10. Fase 7 — Like, Comment, View, Notifikasi

### Tujuan
Semua interaksi sosial dan notifikasi in-app.

### Task

1. **Buat `api-gateway/handlers/social.go`**

   **Like**
   - `POST /api/videos/:id/like`:
     - Cek video accessible & `READY`.
     - Transaction:
       - `InsertLike`.
       - Jika row inserted → `IncrementLikesCount`.
       - Insert notification ke owner jika `actor != owner`.
     - Response `liked=true`, `likes_count`.
   - `DELETE /api/videos/:id/like`:
     - Transaction: `DeleteLike`, jika row ada → `DecrementLikesCount`.
     - Response `liked=false`, `likes_count`.

   **Comment**
   - `POST /api/videos/:id/comments`:
     - Validasi `content` 1–1000 karakter.
     - Video accessible & `READY`.
     - Transaction:
       - `InsertComment`.
       - `IncrementCommentsCount`.
       - Notification ke owner jika bukan owner.
     - Return `201`.
   - `GET /api/videos/:id/comments`:
     - List semua komentar flat, `created_at DESC`, cursor pagination.
     - Video harus accessible.
   - `DELETE /api/comments/:id`:
     - Owner only.
     - Transaction:
       - `CountCommentSubtree`.
       - `DeleteCommentByID`.
       - `DecrementCommentsCount` dengan jumlah subtree.
     - Response `204`.
   - `POST /api/comments/:id/reply` (P1):
     - `GetCommentByID(parent)`.
     - Pastikan parent ada dan `parent.VideoID` sesuai video.
     - Video harus accessible & `READY`.
     - `InsertComment` dengan `parent_id`.
     - `IncrementCommentsCount`.
     - Notification: ke video owner; jika parent author != actor, kirim juga ke parent author.
     - Return `201`.

   **View**
   - `POST /api/videos/:id/view`:
     - Video accessible & `READY`.
     - `IncrementViews`.
     - Response `views_count`.

2. **Buat `api-gateway/handlers/notification.go`**
   - `GET /api/notifications` → `ListNotifications` + pagination.
   - `PUT /api/notifications/read-all` → `MarkAllNotificationsRead`, return `updated_count`.

3. **Routes tambahan**
   ```go
   api.POST("/videos/:id/like", socialHandler.LikeVideo)
   api.DELETE("/videos/:id/like", socialHandler.UnlikeVideo)

   api.POST("/videos/:id/comments", socialHandler.CreateComment)
   api.GET("/videos/:id/comments", socialHandler.ListComments)
   api.DELETE("/comments/:id", socialHandler.DeleteComment)
   api.POST("/comments/:id/reply", socialHandler.ReplyComment)

   api.POST("/videos/:id/view", socialHandler.TrackView)

   api.GET("/notifications", notificationHandler.List)
   api.PUT("/notifications/read-all", notificationHandler.MarkAllRead)
   ```

### Best Practice Fase 7
- Like/unlike dan comment count diupdate dalam **satu transaksi database**.
- Semua insert notifikasi dilakukan di transaction yang sama dengan operasi utama.
- Idempotent: like yang sudah like, unlike yang belum unlike, tetap sukses.

---

## 11. Fase 8 — Delete Account, Delete Video, Cleanup R2

### Tujuan
Memenuhi US-09 dan FR-USER-03: hapus akun beserta semua data lokal dan file R2. Juga cleanup video `DELETED`.

### Task

1. **Buat `api-gateway/handlers/account.go`**

   `DeleteUserData(ctx, db, q, userID)`:
   - Transaction:
     1. `ListVideoKeysByUser(userID)` → kumpulkan semua `r2_key`, `hls_prefix`, `thumbnail_key`.
     2. `DecrementLikesForUser` → perbaiki `likes_count` video orang lain.
     3. `DecrementCommentsForUser` → perbaiki `comments_count` video orang lain.
     4. `DeleteVideosByUser` → hapus video user (cascade menghapus like/comment di video tsb).
     5. `DeleteFollowsByFollower`, `DeleteFollowsByFollowee`.
     6. `DeleteLikesByUser`.
     7. `DeleteCommentsByUser`.
     8. `DeleteNotificationsForUser`, `DeleteNotificationsByActor`.
     9. `TombstoneUser` → `is_active=false`, `deleted_at`, PII null, username placeholder.
   - Setelah commit, enqueue `cleanup:objects` dengan semua R2 keys yang dikumpulkan.
   - Response `204`.

   `DELETE /api/users/me`:
   - Panggil `DeleteUserData(currentUser.ID)`.

2. **Update `api-gateway/handlers/webhook.go`**
   - Event `user.deleted` → panggil `DeleteUserData(userID dari event)`.
   - Event `user.deactivated` → `DeactivateUser`.

3. **Update `transcoder-worker/cleanup.go`**
   - `HandleCleanupObjects`: delete R2 objects dari payload.
   - `HandleCleanupVideo`: seperti Fase 5, untuk video yang `status='DELETED'`.

### Best Practice Fase 8
- PII dihapus langsung; file R2 boleh async maksimal 24 jam sesuai NFR-13.
- Jangan hapus baris `users` — pakai tombstone agar JWT lama tidak membuat user baru.
- Worker cleanup harus idempotent: object yang sudah tidak ada tidak error.

---

## 12. Fase 9 — Wiring API Gateway, Error Handler, Main

### Tujuan
Semua handler ter-register, error mapping seragam, binary bisa jalan.

### Task

1. **Buat `api-gateway/main.go`**
   - Load config.
   - Connect `pgxpool`, `asynq.Client`, `R2Client`.
   - Inisialisasi `db.Queries`.
   - Inisialisasi semua handler struct.
   - `routes.go` register semua route.
   - Jalankan Echo di port `8080`.

2. **Buat custom HTTP error handler**
   - Gunakan `shared.RespondError`.
   - `e.HTTPErrorHandler` mengubah `*shared.Error`, `echo.HTTPError`, dan error biasa menjadi envelope contract.
   - Binder default + validator:
     ```go
     e.Validator = &RequestValidator{Validator: validator.New()}
     ```

3. **Buat `api-gateway/middleware/ratelimit.go`**
   - Rate limiter sederhana untuk endpoint Actions V2 target (`/api/webhooks/zitadel`), misal 10 request/menit per IP.
   - Gunakan `golang.org/x/time/rate`.
   - Return `429 RATE_LIMITED`.

4. **Pastikan `.env` bisa dibaca**
   - Saat development: gunakan `github.com/joho/godotenv` optional.
   - Saat docker-compose: env dari file `.env`.

5. **Dockerfile API Gateway**
   - Multi-stage build `golang:1.22-alpine`.
   - Run as non-root user.
   - Expose port `8080`.

### Best Practice Fase 9
- Tidak ada global variable dependency. Semua di-inject lewat constructor.
- Semua response error mengikuti format API Contract.
- Log cukup ke stdout.

---

## 13. Fase 10 — Testing & Security Hardening

### Tujuan
Menjamin keamanan dan fitur P0 siap demo.

### Task

1. **Unit Tests**
   - `shared/cursor_test.go` — encode/decode, invalid input.
   - `shared/mediatoken_test.go` — valid, expired, tampered.
   - `shared/errors_test.go` — mapping error ke HTTP.
   - `shared/r2_test.go` — tes helper/parse object key (jangan panggil R2 asli).
   - `transcoder-worker/ffprobe_test.go` — table-driven test `ValidateMedia` untuk durasi/codec/resolusi valid & invalid.
   - `transcoder-worker/transcode_test.go` — test `buildFfmpegArgs` / parsing output.
   - `api-gateway/middleware/auth_test.go` — mock `TokenVerifier`, test get-or-create.
   - Handler level: gunakan `httptest` + mock service untuk R2/queue jika memungkinkan. Untuk DB, gunakan integration test dengan PostgreSQL asli di docker-compose.
   - Format: **table-driven test**.

2. **Integration/Smoke Test**
   - `scripts/smoke_test.sh`:
     1. `make up`
     2. `curl /healthz`
     3. Login via Zitadel manual / script, ambil token.
     4. `POST /api/videos/upload-intent`
     5. PUT file dummy ke R2 via `upload_url`
     6. `POST /api/videos/confirm`
     7. Polling `GET /api/videos/:id/status` sampai READY.
     8. `GET /api/videos/:id` dan `GET /api/videos/:id/playlist.m3u8`.
     9. Like, comment, follow, notifikasi.
     10. Delete comment, delete video, delete account.
   - Test cleanup R2 bisa manual: lihat bucket setelah 24 jam.

3. **Security Hardening Checklist**
   - [ ] `ffprobe` validasi sebelum FFmpeg (SEC-01).
   - [ ] Worker non-root, read-only FS, cap drop, timeout (SEC-02).
   - [ ] Redis internal-only + password; job divalidasi di worker (SEC-03).
   - [ ] Object-level authorization di semua endpoint video/comment/sosial (SEC-04).
   - [ ] Signature Actions V2 target diverifikasi via `actions.ValidateRequestPayload` + rate limit (SEC-05).
   - [ ] R2 bucket private, semua akses via presigned URL/signed playlist (SEC-06).
   - [ ] JWT via `zitadel-go` (`oauth.WithJWT`, validasi lokal pakai JWKS), HTTPS only, HSTS (SEC-07).
   - [ ] Upload size divalidasi di confirm + cleanup object invalid (SEC-08).
   - [ ] PostgreSQL worker role terbatas.
   - [ ] `.env` tidak ter-commit, semua default password diganti.

4. **Demo Polish**
   - Pastikan feed p95 < 500 ms, like < 200 ms, upload-intent < 300 ms.
   - Cek disk VPS tidak penuh.
   - Siapkan Postman collection atau curl commands.

---

## 14. Checklist Akhir

- [ ] Semua nama tabel/kolom konsisten dengan ERD.
- [ ] Semua response dan error code konsisten dengan API Contract.
- [ ] Tidak ada GORM.
- [ ] Tidak ada logika refresh token di Go.
- [ ] Semua media private (HLS/thumbnail) tidak pernah URL statis.
- [ ] User delete/deactivated dari Zitadel webhook mempengaruhi data lokal.
- [ ] Worker tidak pernah publish hasil transcode untuk video yang sudah `DELETED`.
- [ ] Migration forward-only, tanpa rollback.
- [ ] Build sukses: `make build`.
- [ ] Unit test lewat: `make test`.

Plan ini siap dieksekusi. Jika ada satu keputusan yang harus diubah di tengah jalan, kembalilah ke PRD ini dan catat perubahannya sebagai asumsi baru.
