# Handoff — Domain-Asli Lokal (fase migrasi) → Prod-VPS next

State captured 2026-09-09, post domain-migration lokal (DOMAIN-MIGRATION-LOCAL-PROMPT.md).
Fase 10 + refactor-testability (PR #52) + PR #54 semua merged.

## Go DM client SDK + CLI (ditambahkan sesi ini)

- `clients/dm` dan `clients/dm/cmd/dmcli` menyediakan client Olm v1 referensi:
  register device, upload signed one-time keys, key bundle + fingerprint
  verification, encrypt/decrypt, inbox poll, ack retry, dan pickle state
  terenkripsi (`0600`).
- Dependensi kriptografi: `maunium.net/go/mautrix v0.30.0`
  (`crypto/goolm`, pure Go, MPL-2.0). Tidak ada cgo/libolm.
- `go build ./...`, `go vet ./...`, `go test ./... -count=1` lulus. Tes client
  memakai relay in-memory (`httptest`), bukan PostgreSQL/Zitadel.
- Smoke live lokal 2026-11-25 memakai gateway `.smoke` + fake OIDC + PostgreSQL:
  dua user saling follow, tukar fingerprint dari key bundle, A→B dan B→A
  berhasil di-encrypt/relay/decrypt/ack. Row unacked berisi 95 byte ciphertext,
  sentinel plaintext tidak ditemukan di DB; hasil `LIVE_DM_E2EE_SMOKE_OK`.
- Flag backend tetap `DM_E2EE_ENABLED=false`; client ini belum melewati
  independent security review/pentest. Jangan klaim production-ready.

## Security review + PR readiness (2026-11-25)

- HIGH diperbaiki: `dmStoreError`/`dmInternal` dulu mengirim detail DB/nama operasi
  ke klien lewat `ClassifyError`; sekarang respons generic `INTERNAL_ERROR`, detail
  tetap di log (`api-gateway/handlers/dm.go:754`,
  `api-gateway/handlers/dm_test.go:498`).
- MEDIUM diperbaiki: rotasi fingerprint ditolak sampai `ForceVerify`/`-force`
  (`clients/dm/service.go:191`), `Send`/`Receive` dimutex untuk pemakaian konkuren,
  `NewClient` menolak `http` ke host non-loopback (`clients/dm/client.go:36`),
  CLI bisa baca plaintext dari stdin, dan trust store hanya mutasi setelah write
  sukses (`clients/dm/store.go:144`).
- Risiko residual: server tidak memverifikasi tanda tangan OTK/fallback (sesuai
  kontrak; client yang memverifikasi), satu device aktif per user, metadata
  terlihat server, rate limit masih in-process per gateway instance, Zitadel
  asli/pentest belum diuji.
- Verified: `go build ./...`, `go vet ./...`, `go test ./... -count=1`,
  `go test -race ./clients/... ./api-gateway/... -count=1`, `go mod verify`, dan
  secret scan; hanya placeholder/example + dummy `.smoke`, private key dan pickle
  key ber-mode `0600`.
- Untuk PR: sertakan `clients/dm`, `go.mod/go.sum`, `api-gateway/handlers/dm.go`,
  `api-gateway/handlers/dm_test.go`, `migrations/009_dm_e2ee.sql`, `README.md`, dan
  `HANDOFF.md`. Aktifkan flag di environment; jangan commit token/pickle key.

## Review follow-up — lima temuan teman (2026-11-25)

- `Receive` mengarantina pesan rusak/decrypt gagal/fingerprint berubah, tidak
  meng-ack pesan tersebut, dan tetap memproses sequence berikutnya; state v1
  dibaca dan dinormalisasi ke v2 tanpa kehilangan session/ack lama.
- `Send` menyimpan request lengkap + hash plaintext ke outbox sebelum request
  pertama. Retry dengan `-message-id` mengirim ulang ciphertext persis sama;
  plaintext/recipient berubah ditolak lokal dengan `ErrOutboxConflict`.
- Rate limit DM dipisah: kontrol (device, upload key, bundle) 20 burst,
  delivery (send/poll/ack) 120 burst 10/detik dan tidak kena limiter auth
  60/menit.
- FK `dm_inbox.sender_device_id` diganti ke `sender_user_id ... ON DELETE
  RESTRICT` di `migrations/010_dm_inbox_sender_fk.sql`, sehingga re-register
  device tidak menghapus pesan yang sudah dikirim.
- Fallback key didukung di `migrations/011_dm_fallback_keys.sql`, handler upload
  (`fallback_key`), key bundle (`fallback_key`), SDK/CLI, dan sqlc. Chat baru
  tetap bisa dimulai saat one-time key habis.
- Tes baru: outbox retry identik, karantina + lanjut, fallback upload/bundle,
  fallback encrypt/decrypt, dan re-register tidak menghapus inbox terkirim.
  `make db-migrate-dm` menerapkan 009–011 + 015; `make db-migrate-marketplace`
  menerapkan 012–014.

## Security follow-up — DM authorization hardening

- `migrations/015_dm_authorization_hardening.sql` menambah `dm_blocks`; blok
  bersifat dua arah untuk eligibility DM dan wajib diapply sebelum binary baru.
- `IsDMRecipientAllowed` menolak pasangan yang salah satu memblokir user lain,
  termasuk saat kontak Marketplace masih aktif; key bundle tidak mengonsumsi
  one-time key kecuali `include_one_time_key=true`.
- Endpoint baru `POST`/`DELETE /api/dm/blocks/:id` menghapus inbox dan
  idempotency pasangan saat blok dipasang. Limiter key bundle dihitung per
  `(caller, target)` agar satu klien tidak menguras one-time key korban.

## Review follow-up — QA verdict (2026-11-25)


- `DeleteUserData` sekarang menghapus semua inbox DM where user sebagai sender
  atau recipient, lalu `DeleteDMDevicesByUser` menghapus device, one-time/fallback
  key, dan idempotency dalam tx yang sama dengan tombstone. Ini berjalan walau
  `DM_E2EE_ENABLED=false`; migrasi 001–015 wajib untuk semua deploy.
- Tes `TestDeleteMe_HappyPathCascadeAndEnqueue` memverifikasi urutan pembersihan
  DM; `TestDeleteMe_DMCleanupFailureIs500` memverifikasi rollback.
- `scripts/smoketest/dm` menjalankan alur register, upload key, send, poll, ack
  terhadap HTTP + PostgreSQL asli, termasuk schema 009–011 + 015, `FOR UPDATE SKIP
  LOCKED`, `ON CONFLICT ... WHERE`, dan ciphertext-only storage. Bukti ada di
  `scripts/smoketest/dm/README.md`.
- `apiDelivery` hanya dibuat saat flag DM aktif; tes route memeriksa enam route
  DM mengikuti flag. Echo secara otomatis memberi middleware group pada
  catch-all 404 buatannya, jadi `NewRouter` mendaftar ulang `/api/*` dan
  `/api/dm/*` tanpa middleware; `TestUnknownAPIRouteSkipsAuthAndRateLimit`
  membuktikan path tak dikenal kembali 404 tanpa auth/rate limit.
- Registrasi `device_id` milik user lain kini `409 DM_DEVICE_STALE`, termasuk
  fallback untuk race `23505`. Send mengunci device pengirim dengan
  `FOR UPDATE` agar re-register konkuren tidak menyisakan FK error 500.
- Tes tambahan: non-mutual follow 404, stale device upload/poll/ack 409, ack
  pesan device lain 404, validasi `after`/`limit`, user nonaktif 401, janitor,
  dan state v1 → v2.
- Smoke concurrent claim diperkuat: transaksi pertama menahan lock, transaksi
  kedua harus mengambil key berbeda dalam 3 detik; cleanup dipindah ke `defer`
  agar gagal di tengah tidak menyisakan data.
- Deploy gate didokumentasikan: migrasi 001–015 wajib sebelum binary baru
  melayani traffic. Keputusan v1 penghapusan sepihak dan drift index 010 lama
  juga dicatat.
- CI ditambahkan di `.github/workflows/ci.yml`: build/vet/test/race/mod verify
  dengan Go dari `go.mod` (1.25.5), job migrasi 001–015 pada PostgreSQL 16, dan
  integration test bertanda `integration`.
- Gate rilis DM, template keputusan produk, dan template persetujuan
  lisensi/audit dirangkum di
  `planning/14_dm_e2ee_release_gate.md`.

## DM E2EE — backend relay only

- Go gateway, migrasi `009_dm_e2ee.sql` + `010_dm_inbox_sender_fk.sql` +
  `011_dm_fallback_keys.sql` + `015_dm_authorization_hardening.sql`, opaque ciphertext relay, key/device
  endpoints, inbox polling/ack, idempotency, TTL janitor, dan feature gate
  tersedia. Compose meneruskan `DM_E2EE_ENABLED` dan `DM_INBOX_TTL`; flag
  default tetap mati dan TTL v1 adalah 168 jam.
- Bukti smoke ada di `scripts/smoketest/dm/README.md`: Go build/vet/test/race,
  HTTP/PostgreSQL relay, ciphertext-only storage, fitur flag on/off, dan Olm
  encrypt→relay→decrypt via SDK integration test. Zitadel asli dan production
  tidak diuji; ini bukan audit keamanan independen.
- Laporan smoke menemukan dua Critical yang kemudian diperbaiki: base64 DM
  kini dikembalikan tanpa padding secara kanonis, dan `make db-migrate`
  menerapkan migration files berurutan. Perintah migrasi forward-only, tanpa
  ledger: gunakan hanya pada database kosong yang disposable, jangan ulangi.
- Respon ganda setelah kegagalan auth dari laporan smoke sudah diperbaiki di
  middleware dan dicakup tes regresi Go.
- Validasi setelah patch auth: `go test ./... -count=1`, `go vet ./...`, dan
  `go build ./...` lulus. Smoke tidak dijalankan ulang dan migration tidak
  diterapkan oleh assistant.
- Atas keputusan pemilik, package `clients/dm_core` dan helper Dart sementara
  di `.smoke/` dihapus. Laporan/log bukti lokal dipertahankan. Backend tetap
  hanya relay ciphertext; client Olm yang melakukan key verification,
  encrypt/decrypt, dan secure local storage harus disediakan terpisah. SDK Go +
  CLI referensi kini tersedia di `clients/dm` (lihat bagian atas), tetapi belum
  melewati independent security review.
- Production flag tetap mati. Belum ada client Olm eksternal terintegrasi,
  independent security review, atau pentest; jangan klaim production-ready.

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

MokiBox production live di VPS multi-tenant `43.157.227.130`,
repo `/www/wwwroot/mokibox` (branch `deploy/aapanel` — bukan main,
sesuai keputusan user: main bebas perubahan yang menyesuaikan aaPanel).

### Topologi
- Edge: aaPanel nginx host (port 80/443) — `mokibox-nginx` TIDAK jalan
  (`docker-compose.prod.yml` overlay: nginx `profiles:[disabled]`,
  gateway publish `127.0.0.1:8180:8080`). Compose selalu eksplisit
  `-f docker-compose.yml -f docker-compose.prod.yml` (dev override
  ter-track TIDAK boleh auto-load di VPS).
- Zitadel: sibling compose `zitadel-compose/` fresh instance
  (volumes bootstrap+postgres-data fresh), Traefik publish
  `127.0.0.1:8181:80`, deny-list DEFAULT (tanpa bypass dev).
- vhost aaPanel: `mokiboxapi.binery.my.id` → 127.0.0.1:8180,
  `auth.mokibox.my.id` → 127.0.0.1:8181 (file
  `/www/server/panel/vhost/nginx/mokiboxapi.binery.my.id.conf`,
  `/www/server/panel/vhost/nginx/auth.mokibox.my.id.conf`;
  HSTS max-age=63072000, X-Content-Type-Options, X-Frame-Options,
  Referrer-Policy; /demo/ alias ke repo; /callback 302).
- TLS: wildcard LE `*.binery.my.id` + apex via acme.sh DNS-01
  (Cloudflare API token), cert di
  `/www/server/panel/vhost/cert/<domain>/{fullchain,privkey}.pem`,
  renewal otomatis (cron acme.sh, ARI window Nov 2026).
  HTTP→HTTPS 301 aktif.
- DNS: `mokiboxapi` + `auth` A-record 43.157.227.130 **DNS-only** (grey).
  `mokibox` (frontend) belum dibuat — frontend memang di-host terpisah.
  Zitadel domain baru: `auth.mokibox.my.id` (migrasi dari `auth.binery.my.id`
  2026-09-24, fresh re-init Zitadel + re-provisioning penuh).

### Provisioning (Zitadel v4.16.0, script `scripts/zitadel-provision-prod.sh`)
- Project mokibox + apps: `mokibox_web` (confidential BASIC),
  `mokibox_spa` (PKCE public), `mokibox_api` (JWT audience).
- Actions V2: target `mokibox-webhook` →
  `https://mokiboxapi.binery.my.id/api/webhooks/zitadel` (Opsi W,
  tanpa deny-list bypass), execution `user.deactivated`.
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
- R2 CORS origin `https://mokiboxapi.binery.my.id` terverifikasi
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


## Domain-asli lokal (2026-09-09) — AKTIF

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
   domain-asli + demo UI mount.
3. Provisioning: lihat "Domain-asli lokal" di atas + gotcha 1-3.
   Semua ID/secret/signing key → `.env`; regen
   `deploy/demo/config.js` dari `ZITADEL_SPA_CLIENT_ID`.
4. `bash scripts/integration_test.sh` — hostname sekarang
   env-driven (baca `API_BASE_URL`/`ZITADEL_ISSUER_URL` dari
   `.env`; ADMIN_LOGIN/ADMIN_PASS overridable env var).
5. Demo UI browser: `http://mokiboxapi.binery.my.id/demo/`.


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

## Marketplace v1 — local backend

- Implemented the backend-only Marketplace scope in
  `planning/15_marketplace_v1.md`: listing CRUD/browse, private R2 photo
  uploads, favorites, reports, and persistent buyer/seller contact grants
  integrated with existing E2EE DM authorization.
- Migrations `012_marketplace.sql`, `013_marketplace_cleanup_outbox.sql`, and
  `014_marketplace_location.sql` are required by this gateway version,
  including account deletion, DM grant queries, durable cleanup retry, and
  location-radius browse. Apply them only to a local disposable database that
  already has migrations `001`-`011`; do not rerun after successful application.
- Local HTTP/PostgreSQL smoke instructions are in
  `scripts/smoketest/marketplace/README.md`. The script requires two active
  local test accounts/tokens and deletes its uniquely marked listing afterward.
- Verification completed: `make sqlc-gen`, `go test ./... -count=1`,
  `go test -tags=integration ./... -count=1`, `go vet ./...`,
  `go vet -tags=integration ./...`, `git diff --check`, and OpenAPI JSON parse.
- Verification also completed against a disposable local PostgreSQL and
  gateway: migrations `001`-`014` applied, tagged SQL integration tests passed,
  `make smoke-marketplace-local` passed including radius browse, idempotent
  report, contact revocation, and outbox cleanup, and `make smoke-marketplace-live`
  passed against the local stub gateway with an explicit
  `local-simulation` warning. R2 calls are stubbed in tests; no real R2
  operations were performed. No commit, PR, deployment, or production migration
  was made.
- Cleanup enqueue failures are now recorded in `marketplace_cleanup_outbox`
  inside the same transaction as the row/object deletion. The api-gateway
  janitor retries with capped exponential backoff, quarantines rows after 10
  attempts, and sweeps expired pending image intents on the same interval
  (`MARKETPLACE_SWEEP_INTERVAL`, `MARKETPLACE_SWEEP_BATCH`).
- Routes are gated by `MARKETPLACE_ENABLED` (defaults to false in config,
  `.env.example`, and docker-compose; enable only for local smoke or after
  security review). Marketplace
  request bodies now use the central validator while keeping explicit
  cross-field checks for `reason=other`.

## Smoke test gates before PR/merge

- Local smoke (PR gate, rerun after the janitor/outbox and location changes):
  apply migrations `001`-`014` to a disposable PostgreSQL, then run
  `make smoke-marketplace-local DATABASE_URL=...`. It verifies radius browse,
  expired image-intent deletion, and their keys reaching
  `marketplace_cleanup_outbox`.
- Live/staging smoke (required before production): repeat the same HTTP flow on
  a disposable staging database with real R2 and Redis, confirm
  `cleanup:objects` actually deletes the temporary/final object keys, and
  confirm failed enqueues back off instead of leaking objects. Keep
  `MARKETPLACE_ENABLED=false` until this smoke and the security review pass.

## Marketplace anti-abuse review follow-up

- Marketplace contact grants expire after 30 days and carry `revoked_at`.
  Eligibility requires an active, unrevoked, unexpired grant; revoked grants
  stay sticky for that listing, while contacting again renews an expired
  non-revoked grant. `DELETE /api/marketplace/listings/:id/contact` lets either
  party revoke. Queued ciphertext and matching DM idempotency rows are purged
  only when no other active grant or mutual-follow still authorizes the pair.
- Daily quotas cap listings, reports, and contacts at 20 per user per rolling
  24 hours. Count-plus-insert runs in one transaction under a Postgres advisory
  lock, so concurrent replicas cannot race past the quota. Marketplace writes
  also use a dedicated 12/minute burst-10 limiter on top of the generic
  authenticated-user limiter. Reports require details when `reason=other`, and
  duplicate reports stay idempotent.
- Uploaded image bytes are validated and promoted from the temporary presigned
  key to a unique immutable final key that the client never received a
  presigned URL for; the temporary object and any losing retry key are queued
  for cleanup, so a failed concurrent confirm cannot delete the winning object.
- DM fake grants are pair-keyed in tests. Coverage proves strangers are denied,
  revocation denies both directions, quota violations return
  `429 RATE_LIMITED`, expired grants renew, revoked grants stay denied, and
  revocation only purges when no other authorization remains.
- CI now applies migrations 012-014, asserts the new contact, cleanup outbox,
  and coordinate columns, and runs the tagged SQL integration tests against the service
  PostgreSQL, including a concurrent 21-request quota test that must commit
  exactly 20 listings.
- Unit tests now cover the handler ownership/visibility matrix, viewer/owner
  identity in query parameters, janitor retry/backoff/quarantine paths, and
  config/route feature-flag parsing.
- Remaining before production: run the live/staging smoke with real R2+Redis,
  expand the SQL keyset/unique-constraint matrix, and complete independent
  security review. The local PR gate is complete. UI, checkout, payments,
  shipping, and production rollout remain out of scope.


