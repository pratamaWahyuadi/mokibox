# MokiBox — TikTok Clone Backend MVP

Backend ala TikTok untuk MVP. Deploy di satu VPS via `docker compose`.

Modul Go: `github.com/pratamaWahyuadi/mokibox`.

Dokumen acuan:
- `planning/PRD.md` — Product Requirements + Technical Blueprint
- `planning/03_schema.md` — ERD & database schema
- `planning/04_api_contracts.md` — API contract (request/response)
- `planning/LLD_PLAN.md` — Low-Level Design (fase per fase)
- `planning/ISSUES.json` — daftar issue per fase

---

## Fase 0 — Quick start (dev lokal)

```bash
cp .env.example .env
# edit .env: ganti semua "change-me-*" dan "replace-with-*" dengan nilai riil

# 1) start postgres + redis saja dulu
make db-up

# 2) bootstrap role tiktok_api & tiktok_worker
make db-bootstrap

# 3) (Fase 1) apply migrations/001_init.sql
make db-migrate

# 4) hidupkan semua service
make up
```

Perintah lain:
- `make build` — build `bin/api-gateway` dan `bin/transcoder-worker`
- `make sqlc-gen` — regenerate `shared/db` dari `sqlc/queries.sql`
- `make test` — jalankan unit test
- `make logs` — tail log semua container
- `make help` — daftar semua target

---

## Arsitektur singkat

```
Client ──HTTPS──> Nginx (80/443)
                    ├─ /api/    -> api-gateway:8080   (Go + Echo)
                    ├─ /zitadel/-> zitadel:8080       (self-hosted IDP)
                    └─ /healthz -> api-gateway:8080/healthz
                              │
            ┌─────────────────┼─────────────────┐
            │                 │                 │
       PostgreSQL        Redis + Asynq    Zitadel self-host
       (internal)        (internal)       (internal, behind Nginx)
```

`transcoder-worker` tidak pernah expose port. Ia cuma konsumsi job dari
Redis (Asynq).

## Direct-message E2EE relay

Go gateway menyediakan key/device endpoints dan relay ciphertext opaque
(`DM_E2EE_ENABLED=false` secara default). Terapkan migrasi DM dengan
`make db-migrate-dm` (009–011: relay, sender FK, fallback key) dan migrasi
Marketplace dengan `make db-migrate-marketplace` (012–014: kontak DM, expiry,
revocation, cleanup outbox, dan radius lokasi). Untuk DB yang sudah punya 009,
pakai `make db-migrate-dm-existing` (010–011). Migrasi 009–014 harus diterapkan di semua
deploy walaupun flag DM mati, karena penghapusan akun selalu membersihkan data
DM/Marketplace dan eligibility DM membaca tabel kontak. Ini adalah gate deploy:
jalankan migrasi sebelum binary baru melayani traffic; kalau tidak,
`DELETE /api/users/me` akan 500 saat tx cleanup menyentuh tabel yang belum ada.

DB yang sudah menerapkan revisi lama migrasi 010 mungkin masih memiliki index
`idx_dm_inbox_sender_user_id` yang duplikat. Ini hanya drift, tidak merusak;
hapus manual dengan `DROP INDEX IF EXISTS idx_dm_inbox_sender_user_id;` jika
ingin rapi. Lihat [desain backend DM E2EE](./planning/13_dm_e2ee_design.md),
[gate rilis DM](./planning/14_dm_e2ee_release_gate.md), dan
[smoke DM](./scripts/smoketest/dm/README.md).

CI menjalankan build, vet, test, race test, verifikasi modul, migrasi 009–014
pada PostgreSQL 16, dan integration test bertanda `integration` untuk setiap
PR; definisinya ada di `.github/workflows/ci.yml`.

### Marketplace v1 (lokal)

Backend Marketplace v1 menyediakan listing, pencarian/filter termasuk radius
lokasi ala Facebook Marketplace, foto lewat presigned R2, favorit, laporan
manual, dan izin kontak ke DM E2EE yang ada. Transaksi/pembayaran disepakati
pembeli dan penjual di luar backend. Checkout, shipping, dashboard moderasi,
dan UI belum termasuk. Lihat [scope Marketplace](./planning/15_marketplace_v1.md).
Sebelum menjalankan gateway versi ini, terapkan migration 012–014 satu kali
pada database lokal yang sudah memiliki schema 001–011 dengan
`make db-migrate-marketplace`; semuanya forward-only dan diperlukan juga oleh
account deletion serta izin DM.
Route Marketplace dikontrol `MARKETPLACE_ENABLED` (default false di config,
`.env.example`, dan docker-compose; aktifkan hanya untuk smoke lokal/setelah
review selesai), dengan janitor
`MARKETPLACE_SWEEP_INTERVAL=1m` dan `MARKETPLACE_SWEEP_BATCH=100` untuk
menyapu image intent kedaluwarsa dan retry cleanup R2 dari outbox.
Jangan terapkan pada production dalam scope lokal ini. Semua write Marketplace
memakai limiter 12/minute burst 10; pembuatan listing, laporan, dan kontak
juga dibatasi 20 per user per 24 jam dengan kunci advisory lock di database.
Izin kontak berlaku dua arah selama 30 hari, dapat dicabut, dan diperbarui
saat buyer menghubungi seller lagi setelah kedaluwarsa.

Gate sebelum PR: jalankan `make smoke-marketplace-local DATABASE_URL=...`.
Gate sebelum production: ulangi smoke live/staging dengan R2+Redis nyata untuk
memastikan objek cleanup terhapus dan enqueue gagal melakukan backoff. Lihat
[HANDOFF.md](./HANDOFF.md#smoke-test-gates-before-prmerge).

### Go client SDK + CLI

`clients/dm` berisi SDK Go untuk Olm v1 (pure Go via `mautrix/crypto/goolm`),
dan `clients/dm/cmd/dmcli` adalah CLI referensi. Client melakukan key
verification, encrypt/decrypt, dan menyimpan private state dalam account/session
pickle terenkripsi; server tetap hanya melihat ciphertext opaque. `upload-keys`
juga mempublikasikan signed fallback key, `send -message-id <uuid>` mengirim
ulang ciphertext persis sama dari outbox lokal, dan `poll` menampilkan pesan
yang dikarantina tanpa menghentikan pesan berikutnya.

Sebelum memakai CLI, aktifkan `DM_E2EE_ENABLED=true` di backend dan siapkan:

- `MOKIBOX_DM_TOKEN` atau `MOKIBOX_DM_TOKEN_FILE` (JWT Zitadel)
- `MOKIBOX_DM_PICKLE_KEY` atau `MOKIBOX_DM_PICKLE_KEY_FILE` (base64 32 byte;
  bisa dibuat dengan `init -generate-pickle-key`, lalu simpan di secure storage)
- `MOKIBOX_DM_BASE_URL` (opsional, default `http://localhost:8080`)

Contoh alur untuk dua akun yang sudah saling follow:

```sh
go run ./clients/dm/cmd/dmcli init -user-id <user-a-uuid> -generate-pickle-key
go run ./clients/dm/cmd/dmcli register
go run ./clients/dm/cmd/dmcli upload-keys -count 50

go run ./clients/dm/cmd/dmcli -state b-state.json -trust b-trust.json init -user-id <user-b-uuid> -generate-pickle-key
go run ./clients/dm/cmd/dmcli -state b-state.json -trust b-trust.json register
go run ./clients/dm/cmd/dmcli -state b-state.json -trust b-trust.json upload-keys -count 50

go run ./clients/dm/cmd/dmcli fingerprint -user-id <user-b-uuid>
go run ./clients/dm/cmd/dmcli verify -user-id <user-b-uuid> -device-id <device-b-uuid> -fingerprint "<fingerprint>"
go run ./clients/dm/cmd/dmcli send -to <user-b-uuid> -text "halo"

go run ./clients/dm/cmd/dmcli -state b-state.json -trust b-trust.json poll
```

`init` menulis state file dengan mode `0600`; pickle key sengaja tidak ditulis
ke file. `send` menolak peer yang belum diverifikasi dan memblokir jika
fingerprint berubah. Tes `go test ./clients/...` memakai relay in-memory, bukan
PostgreSQL/Zitadel; ini bukan security review produksi.

---

## Setup Zitadel self-host (manual, satu kali)

Setelah `make up` pertama kali, lakukan langkah berikut via
**Zitadel Console** di `https://auth.example.com/ui/console/`.
Semua langkah ini tidak bisa di-otomasi dari kode karena Zitadel
Console adalah UI admin yang mengikat setup ke instance yang baru
dibuat.

### 1. Selesaikan first-instance setup
Buka `https://auth.example.com/ui/console/`. Ikuti wizard:
- Pilih region: terdekat (mis. `asia-southeast-1`)
- Masukkan nama instance, mis. `mokibox-prod`
- **Simpan** master key (juga sudah di-set di compose sebagai
  default `PleaseChangeMeZitadelMasterKey` — ganti untuk produksi)

### 2. Buat Organization
- Default organization sudah ada (`Default`); boleh dipakai atau
  buat baru: `Organization > New`
- Catat nama org; semua user default masuk ke sini

### 3. Buat Project untuk backend
- `Projects > New Project`, mis. `mokibox-backend`
- Di project ini nanti dibuat 2 application (Web/OIDC dan API)

### 4. Application #1 — Web/OIDC (untuk login)
- `Projects > mokibox-backend > New Application`
- Type: **Web**
- Name: `mokibox-web`
- Authentication: **Code**
- Redirect URIs: `https://api.example.com/api/auth/callback`
  (dan untuk Postman di dev: `https://oauth.pstmn.io/v1/callback`)
- Post logout redirect: `https://api.example.com`
- Setelah dibuat, salin **Client ID** ke `.env` sebagai
  `ZITADEL_CLIENT_ID`. Buat client secret dan simpan di secrets
  manager (tidak dipakai oleh backend Go; hanya oleh client
  yang melakukan code flow, yaitu Postman / mobile app nanti)

### 5. Application #2 — API (resource server)
- Di project yang sama, `New Application`
- Type: **API**
- Name: `mokibox-api`
- Di tab **Token Settings**:
  - **Auth Token Type = JWT** ← wajib (default-nya opaque)
  - Tambahkan audience: `mokibox-api` (atau nama lain; yang
    penting konsisten dengan `ZITADEL_API_CLIENT_ID` di backend)
- Salin **Client ID** ke `.env` sebagai `ZITADEL_API_CLIENT_ID`

### 6. Aktifkan Google Sign-In
- `Settings > Identity Providers > Google`
- Ikuti wizard Google Cloud Console:
  1. Buat OAuth 2.0 Client (Web application) di Google Cloud
  2. Authorized redirect URI:
     `https://auth.example.com/idps/callback/v2/<google_idp_id>`
     (Zitadel akan menampilkan URI ini setelah form Google dibuat)
  3. Tempel Client ID + Client Secret ke Zitadel
- `Settings > Login Behavior > Identity Providers`: aktifkan
  tombol Google di login page

### 7. Actions V2 — Target untuk webhook lifecycle
- Buka `https://auth.example.com/ui/console/actions`
- `Targets > New Target`:
  - Name: `mokibox-webhook`
  - Type: **Webhook**
  - Endpoint URL: `https://api.example.com/api/webhooks/zitadel`
  - Timeout: 10s
- Klik **Create**. Zitadel akan menampilkan **Signing Key**
  di response. **Salin dan simpan** sebagai
  `ZITADEL_TARGET_SIGNING_KEY` di `.env`. (Backend Go akan
  memverifikasi HMAC `ZITADEL-Signature: t=<ts>,v1=<hmac_hex>`
  memakai `github.com/zitadel/zitadel-go/v3/pkg/actions`
  `actions.ValidateRequestPayload`.)

### 8. Actions V2 — SetExecution untuk lifecycle event
Kita butuh dua execution, satu per event:
- `Executions > New Execution`
  - Condition: `user.removed`
  - Targets: pilih `mokibox-webhook` (dari langkah 7)
  - Create
- Ulangi untuk `user.deactivated`

> **Catatan payload.** Actions V2 mengirim field `userID`
> (bukan `user_id`). Handler webhook di backend memakai field ini.

### 9. Konfirmasi issuer
Pastikan `ZITADEL_ISSUER_URL` di `.env` persis sama dengan
domain publik yang dipakai Zitadel Console (default:
`https://auth.example.com`). Backend Go akan memanggil
`${ZITADEL_ISSUER_URL}/.well-known/openid-configuration` untuk
discovery + JWKS.

---

## Verifikasi cepat

```bash
# health
curl -k https://api.example.com/healthz

# Zitadel reachable via nginx
curl -k -I https://auth.example.com/ui/console/

# jalankan smoke test end-to-end (Fase 10)
./scripts/smoke_test.sh
```

---

## Catatan keamanan (ringkas)

- Hanya Nginx yang publish port 80/443 ke host. PostgreSQL, Redis,
  dan Zitadel **hanya** di internal network.
- Redis dijalankan dengan `--requirepass` dan `--appendonly yes`.
- Worker transcoder jalan sebagai non-root UID 10001, read-only
  filesystem, `cap_drop: ALL`, `no-new-privileges`, `tmpfs /tmp`,
  `pids_limit 200`. Hardening penuh dipasang di Fase 5.
- Upload video **tidak** lewat Nginx; client PUT langsung ke
  presigned URL Cloudflare R2. Nginx body cap 1 MB.
- Semua akses media HLS/thumbnail via presigned URL atau signed
  playlist (lihat Fase 6).
