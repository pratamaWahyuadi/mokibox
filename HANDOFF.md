# Handoff — Fase 10 FINAL → Prod-Domain Migration

State captured 2026-09-04, post PR #47 + #48 + #49 merged
(semua 6 issue fase 10 closed: #29 #30 #31 #39 #44 + #28
via PR #45).
Regenerate per session via `git fetch && gh pr list && gh issue list --state open`.

## Latest

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
