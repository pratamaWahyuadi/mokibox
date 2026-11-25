# Handoff — Domain-Asli Lokal (fase migrasi) → Prod-VPS next

State captured 2026-09-09, post domain-migration lokal (DOMAIN-MIGRATION-LOCAL-PROMPT.md).
Fase 10 + refactor-testability (PR #52) + PR #54 semua merged.

## Profile avatar upload — PR #72

- Backend menyediakan `POST /api/users/me/avatar/upload-intent` dan
  `POST /api/users/me/avatar/confirm` untuk upload langsung ke R2 privat.
- Format JPEG/PNG/WebP, maksimum 5 MiB (divalidasi saat confirm), dengan
  intent ber-expiry; avatar dapat dilihat publik termasuk dari akun privat.
- `avatar_key` aktif/pending disimpan terpisah dari `avatar_url` legacy.
  Update URL legacy, confirm, dan penghapusan akun menjadwalkan cleanup key.
- `PUT /api/users/me` mempertahankan avatar R2 jika klien mengirim balik
  presigned URL avatar milik user sendiri; upload baru tetap memakai
  upload-intent/confirm. Issue acceptance criteria: #73.
- Kontrak API, PRD, schema, dan LLD telah diperbarui. `make sqlc-gen`,
  `go test ./...`, `go vet ./...`, dan build gateway/worker lulus.
- PR untuk review: https://github.com/pratamaWahyuadi/mokibox/pull/72
- Dependency `golang.org/x/image/webp` dipakai untuk memeriksa format WebP
  aktual dari konten upload, bukan hanya mempercayai header `Content-Type`.
- **Deploy order:** apply `migrations/008_user_avatar_upload.sql` before
  deploying the avatar-aware gateway. The existing `make db-migrate` only
  applies `001_init.sql`; after `002_reconcile_users_grant.sql`, run
  `make db-migrate-avatar`. Migration numbers 003–007 remain reserved by the
  parity roadmap; avatar migration 008 is independent and can be applied now
  to the existing database. The auth middleware reads user rows containing
  the new avatar columns, so deploying the gateway first can break
  authenticated endpoints.
- **R2 orphan trade-off:** an upload-intent that is never confirmed leaves
  its pending object in R2 until another intent replaces it, an invalid
  confirm removes it, or the account is deleted. The reconciler scans avatar
  prefixes only for tombstoned users; it does not yet expire abandoned
  pending uploads for active users.
- **Upload limitations:** a presigned PUT URL can be reused until it expires,
  including after confirm; image dimensions are not currently bounded. The
  5 MiB byte limit and decoded format validation still apply at confirm.
- **Live smoke:** `scripts/smoketest/phase12_avatar` performs real HTTP
  upload/confirm/GET, R2 CORS preflight, DB state checks, and PUT echo
  validation twice. It has not been run here because this checkout has no
  `.env`, configured R2 credentials, or running compose services.
- PR #68 (search) tidak diubah; fitur avatar diajukan sebagai PR terpisah.

## Production deploy VPS (2026-09-10) — AKTIF (prompt 2 selesai)

> **DOMAIN TRIO SAAT INI (2026-11, keputusan user 2026-11-25):**
> backend `https://api.mokibox.my.id`, Zitadel
> `https://auth.mokibox.my.id`, frontend apex
> `https://mokibox.my.id`. Migrasi dari
> `mokiboxapi.binery.my.id` sudah live di VPS (vhost + DNS +
> TLS + R2 CORS origin + target webhook Zitadel baru
> `https://api.mokibox.my.id/api/webhooks/zitadel`).
> Referensi `*.binery.my.id` di bawah dan di dokumen
> riwayat (bagian 2026-09) adalah KONDISI LAMA — lihat
> juga bagian "Riwayat domain API (pre-2026-11)" di bawah.

MokiBox production live di VPS multi-tenant `43.157.227.130`,
repo `/www/wwwroot/mokibox` (branch `deploy/aapanel` — bukan main,
sesuai keputusan user: main bebas perubahan yang menyesuaikan aaPanel).

### Topologi
- Edge: aaPanel nginx host (port 80/443) — `mokibox-nginx` TIDAK jalan
  di VPS (nginx service tidak di-up; gateway publish `127.0.0.1:8180:8080`).
  **Deploy compose di VPS memakai TIGA file eksplisit** (diverifikasi
  dari label `config_files` via `docker inspect`; file
  `docker-compose.prod.yml` TIDAK ADA di repo mana pun):
  `docker compose -f docker-compose.yml -f docker-compose.override.yml -f docker-compose.ports.yml up -d`
- Zitadel: sibling compose `zitadel-compose/` fresh instance
  (volumes bootstrap+postgres-data fresh), Traefik publish
  `127.0.0.1:8181:80`, deny-list DEFAULT (tanpa bypass dev).
- vhost aaPanel SAAT INI (2026-11): `api.mokibox.my.id` → 127.0.0.1:8180,
  `auth.mokibox.my.id` → 127.0.0.1:8181, apex `mokibox.my.id` → SPA
  MokiBox-web (HSTS max-age=63072000, X-Content-Type-Options,
  X-Frame-Options, Referrer-Policy; /callback 302). Riwayat vhost lama
  `mokiboxapi.binery.my.id.conf` (pre-2026-11) sudah dipensiunkan.
  Demo web DIHAPUS, digantikan Swagger UI `/api/docs`.
- TLS SAAT INI: cert per-domain via acme.sh DNS-01 (Cloudflare API
  token) — `api.mokibox.my.id`, `auth.mokibox.my.id`,
  `mokibox.my.id`, wildcard `*.mokibox.my.id` — di
  `/www/server/panel/vhost/cert/<domain>/{fullchain,privkey}.pem`,
  renewal otomatis (cron acme.sh). Riwayat: wildcard LE
  `*.binery.my.id` dipakai sebelum migrasi TLD.
  HTTP→HTTPS 301 aktif.
- DNS SAAT INI: `api`, `auth`, apex `mokibox.my.id` → VPS,
  DNS-only (grey). Riwayat: `mokiboxapi` A-record lama. Zitadel
  domain: `auth.mokibox.my.id` (migrasi dari `auth.binery.my.id`
  2026-09-24, fresh re-init Zitadel + re-provisioning penuh).

### Provisioning (Zitadel v4.16.0, script `scripts/zitadel-provision-prod.sh`)
- Script sekarang env-driven: `ZITADEL_DOMAIN` (default
  `auth.mokibox.my.id`), `API_BASE` (default
  `https://api.mokibox.my.id`) — redirect URI apps +
  endpoint webhook mengikuti `API_BASE`.
- Project mokibox + apps: `mokibox_web` (confidential BASIC),
  `mokibox_spa` (PKCE public), `mokibox_api` (JWT audience).
- Actions V2: target `mokibox-webhook` →
  `https://api.mokibox.my.id/api/webhooks/zitadel` (Opsi W,
  tanpa deny-list bypass), execution `user.deactivated`.
  Riwayat: target pre-2026-11 memakai `mokiboxapi.binery.my.id`.
- Test users test1/test2, password sesuai kontrak integration_test.sh
  (`MokiTest1-A`/`MokiTest2-B`, di-set via
  `POST /management/v1/users/{id}/password` + `noVerification:true`).
- Secrets di VPS `/root/mokibox-secrets/` (client secret, signing key,
  test passwords); .env prod `chmod 600` di repo dir.

### Verifikasi prod (semua hijau 2026-09-24)
- integration_test.sh **16 PASS × 2 run berturut-turut** di VPS
  (healthz, OIDC discovery issuer=https://auth.mokibox.my.id, headless login JWT,
  SPA PKCE, users/me, upload→transcode→READY→playlist 480p,
  feed exclusion, follow feed, webhook real user.deactivated
  (aggregateID), soft delete + cleanup).
- integration_test.sh diperbaiki: `ADMIN_LOGIN` domain update ke
  `zitadel-admin@zitadel.auth.mokibox.my.id` (commit 061f46d).
- R2 CORS origin SAAT INI: `https://api.mokibox.my.id`
  (ditambah saat migrasi 2026-11). Riwayat origin lama
  `https://mokiboxapi.binery.my.id` juga sempat terverifikasi
  (ACAO header muncul di presigned GET — lesson prompt-1 #8 closed).
- /api/users/me 401 envelope; anonymous video-GET 401
  (rate-limit middleware depan — DEViasi dari ekspektasi prompt
  "404 envelope"; bukan bug: rate limit auth-first, hardening SEC).
- Multi-tenant aman: 9 container tenant lain tak tersentuh;
  memori 4.3Gi/7.4Gi used, mokibox+zitadel ~450Mi total, no swap.
- Worker reconcile ticker jalan tanpa permission error (002 grant ok).

### Gotchas prod (live-verified)
- **login-client PAT TIDAK BISA management API** (403 AUTH-5mWD2);
  pakai sessionToken admin (session via login-client PAT +
  zitadel-admin login) — skill zitadel §5.9.6.
- **User search** = `POST /v2/users` (bukan /v2/users/search, 405);
  field `userId` bukan `id`.
- **Set password user existing** = `POST /v2/users/{id}/password`
  perlu efek pending-verification — v1
  `POST /management/v1/users/{id}/password` + `noVerification:true`
  yang live-works.
- **Zitadel domain migration** = fresh re-init WAJIB (domain ter-catat
  di eventstore DB; volume bootstrap+postgres-data harus dihapus).
  Admin login domain mengikuti external domain:
  `zitadel-admin@zitadel.<external-domain>`. integration_test.sh
  ADMIN_LOGIN harus disync.
- Container env beku: setelah update .env R2, WAJIB
  `docker compose ... up -d --force-recreate` (restart saja tidak
  reload env) — root cause itest run-2 403 massal.
- apt mirror Tencent internal tidak resolv dari publik; swap ke
  archive.ubuntu.com untuk install (ffmpeg/basenc/jq), lalu restore.
  jq 1.7.1 dipasang via static binary GitHub.
- acme.sh CF plugin pakai `CF_Token` (API token baru cfat_*),
  di-pass via stdin, tidak di-file.
- MCP ssh-mcp run/privileged/sftp tidak stabil di sesi ini
  (timeout 300s / tool-call promotion); SSH askpass dari laptop
  (`SSH_ASKPASS` + config password) lebih reliable.

### Security follow-up (WAJIB setelah sesi ini)
- **ROTASI password VPS ubuntu** — ter-expose di transcript sesi
  2026-09-10 (agent redaction gagal).
- **ROTASI/roll Cloudflare API token `cfat_...`** — pernah di-paste
  di chat oleh user.
- CF token ini punya Zone DNS Edit scope binery.my.id — simpan di
  password manager; acme.sh renewal di VPS butuh token aktif.


## Riwayat domain API (pre-2026-11) — Domain-asli lokal (2026-09-09)

> **[RIWAYAT]** Bagian ini mendokumentasikan kondisi saat itu:
> backend `mokiboxapi.binery.my.id`, auth `auth.binery.my.id`,
> frontend `mokibox.binery.my.id`. SAAT INI backend
> `api.mokibox.my.id`, Zitadel `auth.mokibox.my.id`, frontend
> `mokibox.my.id`. Domain trio binery di sini tidak lagi
> dipakai — jangan dijadikan referensi konfigurasi sekarang.

Domain trio (keputusan user 2026-09-04): backend
`mokiboxapi.binery.my.id`, auth `auth.binery.my.id`,
frontend `mokibox.binery.my.id`. Semua verifikasi hijau:

- `/etc/hosts`: `127.0.0.1` + `::1` untuk ketiga domain
  (AAAA record Cloudflare publik ada — TANPA `::1` entry,
  browser IPv6-preferring nyasar ke Cloudflare → error 1033;
  hapus/comment entry saat mulai tes prod).
- Zitadel re-init dengan `ZITADEL_DOMAIN=auth.binery.my.id`
  (HTTP-only, EXTERNALPORT=80). **WAJIB `docker volume rm`
  BUKAN cuma `zitadel_zitadel-bootstrap` tapi JUGA
  `zitadel_postgres-data`** — DB lama masih menyimpan instance
  lama (domain membeku di eventstore); tanpa itu up membawa
  kembali instance localhost. Admin baru:
  `zitadel-admin@zitadel.auth.binery.my.id` / `Password1!`.
- Re-provisioned: org+project MokiBox, apps mokibox_web
  (confidential/BASIC/JWT) + api_app + mokibox_spa
  (UserAgent/NONE/PKCE/devMode/JWT), test1/test2 users,
  Actions V2 target `http://mokiboxapi.binery.my.id/api/webhooks/zitadel`
  + executions user.deactivated/user.removed. Nilai baru semua
  di `.env` (client IDs, secret, signing key).
- integration_test.sh: 16 PASS / 0 FAIL **2x back-to-back**
  (rerun-ability terbukti).
- Webhook aggregateID verifikasi ulang di domain baru:
  log `aggregateID=test2, userID=admin` → tombstone test2 benar.
- HLS chain: master (2 variant, 480p+720p) → variant →
  segment R2 200 + `Access-Control-Allow-Origin:
  http://mokiboxapi.binery.my.id` (bucket CORS origin baru live).
- **Zero Go code changes** — semua via env/config, sesuai desain.

### Gotcha baru (jangan diulang di VPS)

1. **Actions V2 execution payload v4.16**: condition =
   `{"event":{"event":"user.deactivated"}}` — BUKAN
   `{"event":{"eventType":...}}` (skill §5.6 usang; diverifikasi
   dari proto source tag v4.16.0 `EventExecution.event`).
2. **Create user + password dalam satu call**: v1
   `POST /management/v1/users/human` MENGABAIKAN field password →
   user state 6 "not yet initialized" dan `POST .../password`
   ditolak. Yang benar: `POST /v2beta/users/human` dengan
   `{"password":{"password":"...","change_required":false}}`.
3. **Enum AddOIDCApp**: `responseTypes:[0]` = code (bukan 1;
   1 = id_token → implicit → unauthorized_client). Sama untuk
   `grantTypes:[0]`. Kalau salah create, PUT oidc_config bisa
   memperbaiki tanpa recreate (full payload, gotcha subset).
4. **/.well-known/openid-configuration tanpa port**: issuer
   `http://auth.binery.my.id` (EXTERNALPORT=80 dianggap default,
   tidak muncul `:80` di issuer) — cocok dengan `ZITADEL_ISSUER_URL`
   di `.env`.
5. **/etc/hosts butuh baris `::1` juga** (lihat atas) —
   getent/curl pakai IPv4 dulu, tapi browser modern happy-eyeballs
   bisa pilih AAAA publik.

### Runbook lengkap (domain-asli, menggantikan runbook *.localhost)

> CATATAN (2026-11-24): demo web (`deploy/demo/index.html`,
> `deploy/demo/config.js`, alias `/demo/`) sudah [DIHAPUS] dan
> digantikan Swagger UI di `/api/docs` — lihat bagian
> "Swagger UI + login Zitadel". Langkah yang menyebut demo
> di bawah disimpan sebagai riwayat saja.

1. Zitadel stack (`zitadel-compose/`, gitignored):
   - `.env`: `ZITADEL_DOMAIN=auth.binery.my.id`,
     `ZITADEL_EXTERNALPORT=80`, `ZITADEL_EXTERNALSECURE=false`,
     `ZITADEL_PUBLIC_SCHEME=http`, `PROXY_HTTP_PUBLISHED_PORT=8081`.
   - `docker-compose.override.yml` (deny-list narrower) tetap
     diperlukan untuk Actions V2 webhook ke Docker bridge.
   - Fresh init: down + `docker volume rm zitadel_zitadel-bootstrap
     zitadel_postgres-data` + up.
2. MokiBox compose: `docker compose up -d` — override auto-load:
   nginx local.conf (server_name domain-asli) + aliases
   domain-asli. [DIHAPUS] mount demo UI.
3. Provisioning: lihat "Domain-asli lokal" di atas + gotcha 1-3.
   Semua ID/secret/signing key → `.env`. [DIHAPUS] regen
   `deploy/demo/config.js`.
4. `bash scripts/integration_test.sh` — hostname sekarang
   env-driven (baca `API_BASE_URL`/`ZITADEL_ISSUER_URL` dari
   `.env`; ADMIN_LOGIN/ADMIN_PASS overridable env var).
5. [DIHAPUS] Demo UI browser `http://mokiboxapi.binery.my.id/demo/`
   — sekarang digantikan Swagger UI: `https://api.mokibox.my.id/api/docs`
   (riwayat saat runbook ini aktif: `mokiboxapi.binery.my.id`).

### Urutan deploy produksi (VPS, tiga-file compose)

1. **Backup**: `pg_dump` database + salinan `.env` (jangan
   di-commit, simpan di luar repo tree).
2. **Migrasi dulu** (sebelum build gateway baru — middleware auth
   membaca kolom baru): `psql ... -v ON_ERROR_STOP=1 -f
   migrations/00N_*.sql`. Migrasi 008_user_avatar_upload.sql sudah
   diterapkan di produksi 2026-11-24. Migrasi 003-007 dicadangkan
   roadmap parity (jangan diisi nomor lain).
3. **Build & recreate** (hanya service yang berubah):
   `docker compose -f docker-compose.yml -f docker-compose.override.yml -f docker-compose.ports.yml up -d --build --no-deps api-gateway transcoder-worker`
4. **Verifikasi**: `curl http://127.0.0.1:8180/healthz` → 200.


## Latest

- **Handler-testability refactor** (`refactor/handler-testability`,
  2026-09-07, branch dari main post-PR-#51): review Go idiomatic
  menemukan DI berhenti di middleware — 8 handler structs pegang
  concrete `*db.Queries`/`*sql.DB`, handler layer (~3.4k baris)
  punya 0 unit test. 3 batch, semua green:
  - `fix: [testability.1]` — **BUG PRODUCTION ditemukan via RED
    test**: `ClassifyError` map bare `NewAPIError` ke 500 (httpStatusFor
    cuma match sentinel; APIError tanpa cause → default 500). 10 call
    site validasi/size-error selama ini 500-with-correct-code. Fix:
    tabel `httpStatusForCode` di shared/errors.go (keputusan user
    Option A). Integration 16 PASS tidak pernah kena karena tidak
    pernah kirim payload invalid tersebut.
  - `refactor: [testability.1]` — tx.go (txRunner[T] + sqlTxRunner,
    bind-closure compile-time checked, NO `WithTx(tx).(T)` assertion),
    visibility.go (assertVideoOwnerOrVisible = owner bypass SEMUA;
    assertVideoReadyVisible = playlist mode NO owner bypass — dua mode
    eksplisit share sub-step), social.go → interfaces + 5 tx path via
    runner + parseAuthVideoParam return error (0 `_ =` discard),
    21 test. routes.go TIDAK berubah (constructor tetap terima concrete).
  - `refactor: [testability.2]` — queue.go (taskEnqueuer + asynqEnqueuer),
    video.go ConfirmUpload via runner (enqueue DI DALAM tx body —
    draft pertama sempat pindah ke post-commit, tertangkap self-review;
    TestConfirmUpload_EnqueueFailureRollsBack pin ini), account.go
    delete-cascade → deleteUserData + collectUserR2Keys (runner +
    enqueuer), exported DeleteUserData adapter untuk webhook.go
    (call site unchanged), TombstoneUser ErrNoRows → rollback+ack via
    errNoLocalUser sentinel. 14 test. Catatan: prompt bilang video.go
    tx x2; live hanya ConfirmUpload (DeleteVideo single-statement) —
    semua tx path aktual (7 total) via runner.
  - `refactor: [testability.3]` — user/user_follow (struct field R2/
    Queue/Cfg UNUSED dibuang; constructor signature tetap),
    notification/feed/video_detail/webhook → interfaces.
    **GetPlaylist TRAP resolved**: inline visibility →
    assertVideoReadyVisible, 2 behavior di-PIN test:
    non-READY 404 untuk owner sekalipun; private owner via JWT gagal
    IsFollowing(self,self) → 404 (owner harus pakai ?token= path).
    Webhook test pakai REAL actions.ComputeSignatureHeader. 21 test
    (total handler 56; grep assertions 0; integration 16 PASS).
- **PR #51 MERGED** (`a202fca`): playlist auth two-tier — root cause
  400-"CORS" di demo player (hls.js meneruskan Authorization header ke
  segment R2 presigned → SigV4 mismatch; fix = ?token= media token path
  + AuthenticateOptional fail-closed). Lesson di skill
  presigned-object-urls.
- **Fase 10 SELESAI.** `gh issue list --label phase-10 --state open` = kosong.
- **PR #47 `feature/phase-10-tests` MERGED** — unit tests (#29) + FFmpeg
  timeout-kill verification (#39) + 2 bug fix:
  - `9af74d3` phase-10.9: 7 file unit test (cursor/media-token/error-mapping/
    R2 guards/ffmpeg-args/auth-middleware) + UserStore interface refactor
    (consumer-side, `*db.Queries` tetap satisfy, `denyAllVerifier` TIDAK
    di-reintroduce)
  - `9385f16` fix phase-10.10: **markFailedDetached** — semua 4 call site
    `MarkVideoFailed` mewarisi task-ctx yang sudah deadline-exceeded saat
    timeout-kill → row stuck `PROCESSING:3` selamanya. Fix: fresh ctx 30s.
  - `8a4f62f` phase-10.11: runtime smoke `scripts/smoketest/phase10_timeout/`
    (24 PASS, rerun 2x) + `runffmpeg_test.go` (ffmpeg nyata dibunuh @1.01s)
  - `560f15e` phase-10.12: fixture = **sample.mp4 user** (repo root, untracked)
    + **budget self-calibrating** (ukur download R2 aktual → TRANSCODE_TIMEOUT
    = measured + 3s) — WiFi rumah membuat budget fixed tidak mungkin
  - `50e2bba` phase-10.13: `probe_kill_test.go` (ffprobe hang kill, FIFO
    deterministik) + fix `ProbeFile` menelan error asli (kill evidence hilang
    dari log)
- **PR #48 `feature/phase-10-hardening` MERGED** — security hardening (#31):
  - `9d7e3f6` phase-10.14: compose hardening **api-gateway** (read_only +
    cap_drop ALL + nnp + pids 200 + mem 512m) & **nginx** (drop 10 caps keep
    CHOWN/SETGID/SETUID/NET_BIND_SERVICE — attempt pertama drop CHOWN
    crash-loop; readonly + tmpfs + pids/mem) + **SECURITY.md** (audit table
    per-SEC dengan bukti, known limitations, reporting policy)
  - **Rotasi password dev live** (keputusan user): postgres superuser + 2 role
    DB + redis + media-token = `openssl rand`, ALTER live tanpa recreate
    volume, .env + DSN updated. Bukti: integration 16 PASS 2x (post-rotasi +
    post-hardening).
- **PR #49 `feature/phase-10-reconcile` MERGED** — R2 orphan reconciliation
  sweeper (#44):
  - `a13b87f` fix phase-10.15: **migrations/002_reconcile_users_grant.sql**
    — column-level GRANT SELECT (id, is_active, deleted_at) ON users TO
    tiktok_worker. Bug desain live: role worker restricted ke videos (SEC-03)
    → sweeper permission denied. Least privilege: username tetap `f` via
    has_column_privilege. **Production WAJIB apply migration 002 saat deploy.**
  - `730aafc` phase-10.16: `transcoder-worker/reconcile.go` (HandleReconcileTick
    + ticker goroutine di run()), sqlc `ListUsersEligibleForReconcile`,
    `R2Client.ListObjectsByPrefix`, env RECONCILE_INTERVAL=1h / _BATCH=100 /
    _DRY_RUN, `make reconcile-once[-dry]`, smoke 16-assertion
    (`scripts/smoketest/phase10_reconcile/run.sh`, 16 PASS 2x).

## Verification state (semua green saat wrap-up 2026-09-04)

```
go build ./... && go vet ./... && go test ./...   # exit 0 (11 test file)
bash scripts/integration_test.sh                   # 16 PASS / 0 FAIL
bash scripts/smoketest/phase10_timeout/run.sh      # 24 PASS / 0 FAIL, rerun 2x
bash scripts/smoketest/phase10_reconcile/run.sh    # 16 PASS / 0 FAIL, rerun 2x
grep assertions: pgx.ErrNoRows (code) = 0, pgxpool (code) = 0,
                 RouterDeps.SQLDB = 0, denyAllVerifier = 0
```

## Local E2E environment (aktif di VPS dev ini)

Password dev SUDAH dirotasi (bukan change-me-* lagi); integration test
tetap jalan karena semua kredensial dari .env.

Satu quirk baru: **setelah VPS reboot, nginx memegang stale DNS IP
api-gateway** → semua route 502 sampai `docker compose restart nginx`
(nginx resolve upstream sekali saat config load; gateway restart dapat
IP baru). Bukan bug MokiBox — dev-env quirk; dicatat juga di SECURITY.md
known limitations.

### Runbook lengkap (dari issue #30, tetap valid)

Cara menjalankan ulang oleh siapa pun:

1. Zitadel stack (sibling `zitadel-compose/`, gitignored):
   - `.env`: `ZITADEL_DOMAIN=auth.localhost`, `ZITADEL_EXTERNALPORT=80`,
     `ZITADEL_EXTERNALSECURE=false`, `PROXY_HTTP_PUBLISHED_PORT=8081`.
   - `docker-compose.override.yml` (copy dari
     `scripts/zitadel-override.example.yml`): narrows
     `ZITADEL_HTTPCLIENT_DENYLIST` supaya Actions V2 webhook bisa
     menjangkau 172.16.0.0/12 (Docker bridge).
   - `docker compose up -d` (first-init membuat instance fresh; admin
     default: `zitadel-admin@zitadel.auth.localhost` / `Password1!`).
2. MokiBox compose: `docker compose up -d` (docker-compose.override.yml
   auto-load: nginx local.conf + cross-network attach + aliases
   auth.localhost/api.localhost).
3. Zitadel provisioning (sekali): Project `MokiBox` + Web app
   (`mokibox_web`, redirect `http://api.localhost/callback`, BASIC auth +
   generate secret) + API app (`api_app`); set `accessTokenType=1` (JWT)
   via `PUT /management/v1/projects/{pid}/apps/{aid}/oidc_config`
   `{"clientId":"...","authMethodType":1,"accessTokenType":1}`; 2 human
   users test1/test2 + set password via
   `POST /management/v1/users/{id}/password`; Actions V2 target
   `http://api.localhost/api/webhooks/zitadel` + executions
   (user.deactivated, user.removed). Semua ID/secret/signing key → `.env`.
4. `bash scripts/integration_test.sh` — 16 assertions, exit 0 = all pass.

### Dua pola OIDC client (Web vs SPA) — pelajaran penting

| App | application_type | auth_method_type | Token exchange | Untuk |
|---|---|---|---|---|
| `mokibox_web` | 0 (Web) | 1 (BASIC) | client_secret_basic (confidential) | CLI/server-side test |
| `mokibox_spa` | 1 (User Agent/SPA) | 2 (NONE) | PKCE S256 murni, TANPA secret | production frontend browser |

Gotcha: **app tipe Web di Zitadel v4 selalu diperlakukan confidential** —
token endpoint menolak PKCE tanpa secret (`invalid_client: empty client
secret`) walau auth method NONE tercatat. Untuk client public (SPA/Native)
HARUS buat app dengan `application_type=1` (User Agent) atau 2 (Native)
+ `auth_methodType=2` (NONE). Enum v1: appType 0=Web, 1=UserAgent, 2=Native;
authMethodType 0=BASIC, 1=POST, 2=NONE (beda dari asumsi awal saya:
0 bukan NONE!). application_type tidak bisa diubah setelah create —
harus recreate app. `mokibox_spa` dibuat via
`POST /management/v1/projects/{pid}/apps/oidc` dengan
`appType:1, authMethodType:2, devMode:true, accessTokenType:1`
(client id: `389059418009960450`, disimpan di `.env` sebagai
`ZITADEL_SPA_CLIENT_ID`). Step 1b di integration_test.sh membuktikan
pola SPA end-to-end (login → JWT → api-gateway 200).

Catatan headless login (password grant TIDAK didukung Zitadel):
authorize → session v2 (login-client PAT + user password) →
`POST /v2/oidc/auth_requests/{id}` (Bearer login-client PAT) →
code → token (client_secret_basic). Fungsi `headless_login()` di
integration_test.sh adalah referensi implementasinya.


## Reconciliation sweeper — catatan operasional

- Cadence ticker 1h default (env `RECONCILE_INTERVAL`), batch 100 user/tick
  (`RECONCILE_BATCH`), dry-run via `RECONCILE_DRY_RUN=true` atau
  `make reconcile-once-dry`.
- Migration 002 HARUS dijalankan di production sebelum worker versi baru
  start (kalau tidak: sweeper error permission denied tiap tick — task
  akan retry terus; see lesson queue-settle di bawah).
- **Edge case by design (issue Out of Scope)**: user yang dihapus dari
  Zitadel tapi TIDAK PERNAHAH hit API → tidak ada row `users` sama sekali →
  TIDAK tersapu reconciler (butuh row tombstoned untuk masuk query).
  Orphan seperti ini tetap manual: `r2_list`/`ListObjectsByPrefix` + admin.
- Row korup (is_active=false tanpa deleted_at / sebaliknya) di-exclude query
  → admin manual, sesuai issue.
- Load test 1000 user TIDAK dijalankan bulk live (deviasi tercatat di PR #49
  body; struktur LIMIT+batch+tick-budget sudah bounded).

## Decisions & gotchas for prod-migration (fase berikutnya)

- **NEXT WINDOW**: `prompts/PROD-DOMAIN-MIGRATION-PROMPT.md` (untracked,
  ditulis sesi issue #30) — migrasi localhost → domain asli production.
  Eksekusi SETELAH fase 10 closed (sekarang sudah). Pertimbangan pindah
  OIDC provider juga ada di doc itu.
- **Production deploy checklist tambahan (dari fase 10)**:
  1. Jalankan `migrations/002_reconcile_users_grant.sql` (psql pipe).
  2. Regenerate SEMUA secret production (jangan reuse dev yang sudah
     dirotasi — dev values ada di .env lokal yang gitignored; production
     .env terpisah, generate fresh).
  3. Nginx production pakai `default.conf` (TLS + HSTS), BUKAN local.conf;
     override compose (docker-compose.override.yml) jangan ikut production.
  4. Worker + gateway + nginx compose hardening sudah di file — tidak ada
     langkah manual.
  5. Zitadel deny-list override (`ZITADEL_HTTPCLIENT_DENYLIST`) JANGAN
     dipakai di production (itu dev-only untuk Actions V2 webhook SSRF
     bypass — see SECURITY.md + skill zitadel §5.9.3).
- **Counter race** (carry-over fase 5-10): masih no-FOR-UPDATE di decrement.
  #44 reconciliation TIDAK menyelesaikan ini (beda masalah). Kalau mau
  fix, issue terpisah.
- **/healthz unreachable during verifier retry** (fase 9 limitation):
  kalau production mengalami issue, propose /healthz + /readyz split
  sebagai PR terpisah — jangan silent-fix.
- **Per-process rate limit**: single-VPS = satu gateway instance = OK.
  Kalau production scale >1 replica, butuh Redis-backed counter (catatan
  SECURITY.md).
- **No automated security scanning di CI** (trivy/dependabot) — kandidat
  issue follow-up.

## Lessons fase 10 (baru, untuk skill/handoff berikutnya)

1. **Terminal-state writes jangan mewarisi ctx yang bisa expired**
   (phase-10.10): semua write DB yang terjadi SETELAH kill-path harus pakai
   fresh context. Kelas bug sama dengan phase-10.13 (ProbeFile menelan
   error asli). General: "error/ctx asli harus survive sampai log".
2. **Budget fixed vs network variable** (phase-10.12): di uplink WiFi,
   budget hardcoded untuk pipeline yang menyentuh network tidak pernah
   stabil — ukur dulu, set budget = measured + margin.
3. **Task periodic idempotent TETAP bisa lintas-run** (reconcile smoke):
   task yang gagal duduk di asynq retry-set dan dieksekusi kapan syarat
   gagalnya hilang (grant applied) — meracing assertion run berikutnya.
   Smoke state-mutating harus settle queue (drain) sebelum seeding.
4. **Nginx upstream DNS di docker resolve sekali saat config load**
   — setelah gateway restart dapat IP baru, nginx tetap pakai IP lama
   sampai di-restart. VPS reboot = pasti kena. `docker compose restart nginx`.
5. **Column-level GRANT = least privilege yang tepat** untuk sweeper yang
   butuh baca tabel di luar scope role-nya (phase-10.15): grant hanya
   kolom yang dibaca query, PII tetap `f`.
6. **psql -q untuk INSERT..RETURNING di script**: command status
   ("INSERT 0 1") ikut ke stdout tanpa -q dan menggabung dengan value
   RETURNING; plus `tr -d '[:space:]'` untuk CR docker exec.

## Merged (state of main)

- PR #49 (`c7cddd0`) phase-10 sweeper — #44 closed
- PR #48 (`59162a0`) phase-10 hardening — #31 closed
- PR #47 (`60b426e`) phase-10 tests — #29, #39 closed
- PR #46 (`7e49f82`) fase-10 integration smoke — #30 closed (5 bug fix)
- PR #45 (`963cd7b`) fase-9 production wiring — #28 closed

## Alive Branches (JANGAN dijadikan basis)

- `feature/phase-10-tests|hardening|reconcile` — sudah merged, tip-nya
  merge commit. Branch dari `main` saja.

## Files touched fase 10 final (untuk referensi)

```
api-gateway/middleware/auth.go        UserStore interface (testability)
api-gateway/middleware/auth_test.go   11 test (mock verifier + stub store)
shared/{cursor,mediatoken,errors,r2,reconcile}_test.go
transcoder-worker/{ffmpeg_args,runffmpeg,probe_kill}_test.go
transcoder-worker/{transcode,ffprobe,reconcile}.go
migrations/002_reconcile_users_grant.sql
scripts/smoketest/phase10_timeout/    (sample.mp4 fixture + dynamic budget)
scripts/smoketest/phase10_reconcile/   (16-assertion E2E)
docker-compose.yml                     (gateway+nginx hardening, RECONCILE_* env)
SECURITY.md                            (audit per-SEC + limitations + policy)
```

## Issue #57 — GET /api/search (2026-10-04)

**What shipped:** `GET /api/search?q=<query>&type=<all|users|videos>&limit=<n>&cursor=<c>`
— search users and videos with offset-based cursor paging, type filter,
and validation. Endpoint mounted in the authenticated `/api` group in `routes.go`.

**Files touched:**

```
sqlc/queries/search.sql                  NEW — SearchUsers, SearchVideos (:many)
shared/db/search.sql.go                  GENERATED — make sqlc-gen
api-gateway/handlers/search.go           NEW — SearchHandler + handler + mapper + validation + pagination
api-gateway/handlers/search_test.go      NEW — 11 unit tests (happy, 401, empty-q, long-q, limit=0, limit-cap, type-filter, tombstone, pagination, rune-count, underscore)
api-gateway/routes.go                    MODIFIED — api.GET("/search", searchH.Search)
scripts/smoketest/phase11_search/main.go NEW — Real HTTP integration smoke test using real middleware.Authenticate & PostgreSQL (2x back-to-back PASS)
HANDOFF.md                               MODIFIED — this section
```

**Key Fixes & Refinements (from code review):**

- **SQL queries**:
  - Separated `query_pattern` (escaped for `ILIKE`) and `raw_query` (raw string for `LOWER(u.username) = LOWER($raw_query)`) so exact match ranking is preserved for queries with underscores/wildcards (e.g. `pratama_dev`).
  - `v.deleted_at IS NULL` to exclude soft-deleted videos.
  - `u.id = $viewer_id` added to private video visibility condition (owner can search own private videos).
- **Handler & Validation**:
  - `utf8.RuneCountInString` for exact 100-character length check.
  - `escapeSQLWildcards` helper to escape `%` and `_` SQL wildcards in user input.
  - `limit + 1` DB lookahead query to accurately omit `next_cursor` when results are exhausted (prevents false cursors).
  - Strict `strconv.Atoi` cursor parsing with max offset cap (100,000) to prevent int32 overflow.
- **Smoke test**:
  - Uses real `middleware.Authenticate` with a `stubTokenVerifier` for authentication testing (tests real route group wiring & 401 response).
  - Performs direct DB sanity check on seeded dataset before running HTTP assertions.
  - Removed hardcoded DB passwords; requires `DATABASE_URL` from environment.
  - Verifies 401 Unauthorized, 400 validation errors, tombstone exclusion, READY status filter, soft-deleted filter, private owner self-search, underscore exact match ranking, type filters, pagination (including next_cursor omission on last page), and non-null presigned URLs.
  - Runs **2x back-to-back PASS**.

**Deviations from issue:**
- Route registered in `routes.go` (`api.GET("/search", ...)`), following gateway architecture convention.
- Offset paging encoded via base64 `offset:<n>` cursor (issue recommended offset, cursor is an opaque wrapper).
- Video search does NOT exclude viewer's own videos (per issue spec: "user mencari kontennya sendiri itu wajar").

**Known Limitations (per issue):**

- Sequential scan `ILIKE '%...%'` — acceptable for current dataset. Add `pg_trgm` GIN index when users table exceeds ~50k rows.
- Offset paging — acceptable for short search result sets. Switch to keyset/rank paging if result sets grow large.
- Block filter (issue #10) not implemented — only self-exclusion on user search. TODO when block table lands.

**Verification:**

- `go build ./...` — 0 errors
- `go vet ./...` — 0 warnings
- `go test ./...` — all green
- `scripts/smoketest/phase11_search` — PASS 2x back-to-back



## Swagger UI + login Zitadel (pengganti demo web, 2026-11)

Demo web lama (`deploy/demo/index.html`) dihapus; dokumentasi API +
"coba langsung" digantikan Swagger UI:

- Docs di-serve gateway di `/api/docs` via `go:embed`
  (`api-gateway/docs/`: swagger.json, index.html,
  oauth2-redirect.html/js). **Spec dan HTML di-embed ke binary —
  setiap perubahan docs butuh rebuild image gateway**, bukan
  cukup restart.
- Login pakai Authorization Code + PKCE dengan SPA client docs
  (client ID tercatat di `api-gateway/docs/index.html`).
- Redirect URI yang harus terdaftar di Zitadel app:
  - lokal: `http://localhost/api/docs/oauth2-redirect.html`
  - produksi: `https://api.mokibox.my.id/api/docs/oauth2-redirect.html`
- Di dialog Authorize Swagger, **Auth Token Type harus JWT**
  (bukan default opaque) agar access token bisa dipakai
  api-gateway (audience ZITADEL_API_CLIENT_ID).
- Redirect URI ini harus terdaftar di Zitadel app docs; setelah
  migrasi API-domain 2026-11 ke `api.mokibox.my.id`, URI lama
  di app Zitadel perlu diganti/ ditambah (lihat migrasi infra).

## Pelajaran operasional baru (ringkas)

1. **nginx host aaPanel bisa mati sendiri** — kalau semua domain
   tiba-tiba down padahal container sehat, cek dulu service nginx
   aaPanel di host sebelum menyelidiki compose.
2. **Urutan migrasi-sebelum-gateway** — middleware auth membaca
   kolom baru; deploy gateway sebelum migrasi bisa mematikan semua
   endpoint authenticated (lihat deploy order PR #72).
3. **Verifikasi compose via `docker inspect`** — cara sahih memastikan
   file compose apa yang benar-benar dipakai container produksi:
   baca label `config_files`. Jangan percaya dokumen lama
   (docker-compose.prod.yml ternyata tidak pernah ada).
4. **Merge tanpa conflict tetap harus dites** — merge bersih tidak
   menjamin perilaku; jalankan build+test+smoke setelah merge
   sebelum klaim selesai.

## Next work (belum dikerjakan) — HANYA catatan analisis, belum implementasi

### a. Ubah username maksimal sekali per 7 hari

Temuan kode (fakta, belum ada implementasi):

- Kolom: tabel `users` belum punya penanda waktu perubahan username.
  Dibutuhkan mis. `username_changed_at TIMESTAMPTZ NULL` — NULL = belum
  pernah ganti (boleh ganti kapan pun).
- Nomor migrasi kosong berikutnya: **009** (keputusan user:
  003–007 dicadangkan roadmap parity, 008 terpakai avatar —
  fitur username memakai `009_username_change.sql` atau serupa).
- GRANT: role DB `tiktok_api` harus bisa UPDATE kolom baru itu;
  ikuti gaya column-level GRANT seperti `migrations/002_...sql`
  dan `008_user_avatar_upload.sql`.
- Validasi yang ada sekarang (`handlers/user.go` + validator):
  format + unique case-insensitive via constraint/index unique;
  belum ada daftar kata terlarang (reserved words: "admin",
  "mokibox", dll.) — perlu daftar eksplisit.
- Efek ke payload notifikasi: beberapa payload notifikasi menyimpan
  snapshot username (mis. follow) — username lama bisa "membeku"
  di notifikasi lama; dokumen harus menyatakan ini acceptable
  atau read-time join harus dilakukan.
- Respons error saat belum 7 hari: butuh error code baru
  (mis. USERNAME_CHANGE_COOLDOWN) + mapping status di
  `shared/errors.go` — jangan reuse 400 generik tanpa code.
- **Peringatan inkonsistensi dokumen**: dokumen saat ini (UserProfile
  di `planning/04_api_contracts.md`, PRD, dan deskripsi PUT
  /users/me di swagger.json) menyatakan username immutable.
  Implementasi fitur ini WAJIB mengubah tiga dokumen itu juga.

### b. Search & feed tanpa access token (anonim)

Temuan kode:

- `routes.go`: seluruh grup `/api` dipasang
  `middleware.Authenticate` + rate limiter auth-first. Produksi
  punya bukti log "rate limit requires authenticated user".
- `middleware/auth.go`: **AuthenticateOptional sudah ada** (dipakai
  playlist `?token=` path, fail-closed: header absent → anonim,
  header present tapi invalid → 401). Bisa langsung dipakai.
- `middleware/ratelimit.go`: limiter identitas-user (`NewRateLimitAuth`)
  menolak user anonim. Untuk anonim perlu limiter berbasis IP —
  pola referensi `NewRateLimitWebhook` (by-IP) ada; risiko:
  `c.ClientIP()` bisa di-spoof kalau trusted-proxy tidak benar
  (aaPanel nginx → gateway; pastikan X-Forwarded-For diset dan
  dipercaya hanya dari loopback).
- Visibilitas anonim (`visibility.go`): hanya video publik +
  status READY + owner aktif; aturan ini tidak perlu diubah,
  cukup pastikan viewer nil ditangani.
- Nilai field viewer-dependent untuk anonim: `liked_by_me=false`,
  `is_owner=false`, `is_following=false` (jangan null — konsisten
  dengan tipe non-nullable di kontrak).
- Dampak swagger.json: security global berlaku ke semua operasi;
  endpoint anonim baru butuh `security: []` per-operasi
  (`/api/search`, `/api/feed/home`), dan `planning/04_api_contracts.md`
  harus ikut.
- Risiko: scraping/abuse anonim — rate limit IP + pertimbangkan
  batas page-size/cursor ketat untuk anonim.

File yang perlu diubah nanti: `api-gateway/routes.go`,
`api-gateway/middleware/ratelimit.go`,
`api-gateway/middleware/auth.go` (kalau perlu varian optional),
`api-gateway/handlers/search.go`, `api-gateway/handlers/feed.go`,
`api-gateway/docs/swagger.json` (sekalian hapus `VIDEO_NOT_READY`
dari daftar contoh error code — keputusan user: dibiarkan di PR
cleanup, dibersihkan saat fitur ini mengubah spec),
`planning/04_api_contracts.md`.

Usulan tes: unit test rate-limiter by-IP untuk request tanpa auth;
unit test handler search/feed dengan viewer nil; integration
smoke: GET /api/search + /api/feed/home tanpa Authorization → 200
dengan liked_by_me=false; header invalid tetap 401 (fail-closed);
akun privat tidak muncul untuk anonim.
