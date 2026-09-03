# Handoff — Fase 10 (issue #30) → Fase 10 lanjutan

State captured 2026-09-03 (post PR #45 merged, fase-10 issue
#30 branch feature/phase-10-integration-smoke).
Regenerate per session via `git fetch && gh pr list && gh issue list --state open`.

## Latest

- **PR #45 `feature/phase-9` MERGED** (`963cd7b`) — fase 9 production wiring complete.
- **Branch `feature/phase-10-integration-smoke`** (issue #30 + 5 real bug fixes) — 7 commit:
  - `6aa9dfd` phase-10.1: local *.localhost E2E environment (nginx local.conf
    + docker-compose.override.yml + Zitadel denylist override)
  - `2e78987` phase-10.2: **BUG FIX** NewZitadelVerifier issuer-URL parsing
    (pre-existing sejak phase-3.1: URL dobel-scheme → OIDC discovery selalu gagal)
  - `bde9372` phase-10.3: **BUG FIX** CheckToken Bearer prefix
    (pre-existing: token tanpa prefix → ErrMissingToken → 401 untuk semua token valid)
  - `b79fffd` phase-10.4: **BUG FIX** webhook dispatch aggregateID vs userID
    (fase-8: admin-initiated deactivate men-tombstone user yang salah — fatal di production)
  - `ace4279` phase-10.5: **BUG FIX** playlist handler path separator
    (fase-6: key "videoID>master.m3u8" tanpa slash → playlist 404 permanen)
  - `3ba993b` phase-10.6: **BUG FIX** playlist rewrite variant extraction + segment base
    (fase-6: "variant=hls" untuk semua baris + trailing-slash check yang salah)
  - phase-10.7: scripts/integration_test.sh + scripts/zitadel-override.example.yml
- **Integration smoke: 15 PASS / 0 FAIL** (full user journey, live Zitadel).

## Local E2E environment (issue #30, aktif di VPS ini)

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
4. `bash scripts/integration_test.sh` — 15 assertions, exit 0 = all pass.

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

## Merged (state of main)

- PR #43 (`65e0e12`) phase-8 merged — DELETE /api/users/me + DELETE /api/videos/:id + cleanup:video
- PR #42 phase-7, #41 refactor pool-consolidation, #40 phase-6, dst

## Alive Branches (JANGAN dijadikan basis)

- `feature/phase-9` — menunggu review/merge PR #45.
- `feature/phase-0..8` — history; tip-nya merge commit, jangan branch dari sini.
- `refactor/pool-consolidation` — sudah merged.

## Open Items

### Issues GH

- PR #45 body includes checklist issue #28 dengan `Closes #28` (auto-close saat merge).
- Belum disentuh:
  - #29 Fase 10: Unit tests shared utils dan worker
  - #30 Fase 10: Integration smoke test script
  - #31 Fase 10: Security hardening checklist
  - #39 Fase 10: Verify FFmpeg timeout-kill path in worker
  - #44 Fase 10: R2 orphan reconciliation sweeper untuk delete-account dual-write

### Risiko yang ditunda

- **Counter race** (carry-over fase 5-8 + fase 9): tx tidak pakai `SELECT ... FOR UPDATE`
  di decrement. `GREATEST(..., 0)` absorbs. Fase 10 reconciliation job.
  Fase 9 TIDAK fix ini.
- **Live HTTP smoke verification gap** (NEW dari fase 9): fail-fast verifier di Issue A
  + distroless image di Issue D = binary tidak bisa boot tanpa Zitadel infra yang up.
  Issue #30 (integration smoke) menutup gap ini dengan Zitadel real di zitadel-compose.
- **mokibox-nginx restart-loop** (pre-existing): cert belum di-populate di `deploy/nginx/certs/`.
  Fase 9 BUKAN touchpoint nginx. Tinggalkan sebagai known issue di HANDOFF.

## Decisions & gotchas for fase 10+

- **Counter race** (carry-over fase 5-9): tidak ada `FOR UPDATE` di decrement.
  Defer ke fase 10 reconciliation.
- **Orphan R2 risk** (carry-over fase 8): tracked issue #44, fase 10 reconciliation sweep.
- **Verification gap on live HTTP smoke** (carry-over fase 9): binary fail-fast tanpa
  Zitadel infra up. PR body section "Verification gap" explains why integration smoke deferred
  to issue #30. Static checks + grep assertions + Docker build verify wire contract.
- **Fail-fast verifier + retry** (NEW fase 9 + fase 9.6): `denyAllVerifier` removed dari
  `main.go`. Misconfigured ZITADEL_ISSUER_URL → container restart-loop. Cold-start race
  absorbed by 60s retry budget. Trade-off documented.
- **/healthz unreachable during retry** (NEW fase 9.6 caveat): retry-with-backoff absorbs
  cold-start race but /healthz masih unreachable selama retry window (solver
  liveness-vs-readiness split butuh srv.ListenAndServe sebelum verifier build — di luar
  scope fase 9). Kalau fase 10 observe production issue, split ke /healthz (liveness) +
  /readyz (readiness).
- **Distroless image** (NEW fase 9): `gcr.io/distroless/static-debian12:nonroot`. Tidak punya
  shell — `docker exec ... sh` tidak akan jalan. Untuk debug live, harus buat debug image
  terpisah atau pakai `docker debug` (kalau Docker version support).
- **Zero handler refactor** (NEW fase 9): handler fase 3-8 tetap pakai `shared.RespondError`.
  Central HTTPErrorHandler safety net untuk framework errors saja. Kalau fase 10 mau adopt
  `return err` idiom secara konsisten, refactor eksplisit dengan migration plan — jangan
  half-half.
- **`shared.ClassifyError`** (NEW fase 9): exported (dulu `classifyError` unexported).
  Internal call site `RespondError` updated. Kalau fase 10 bikin wrapper error baru,
  tambahkan mapping di `shared/errors.go` `httpStatusFor` + `codeFor`, JANGAN duplicate
  logic di tempat lain.
- **Rate limit per-process `sync.Map`** (NEW fase 9): janitor 5 menit sweep TTL 10 menit.
  Multi-instance deployment (kalau fase 10 atau production scale): per-instance counter,
  bukan global. Untuk global rate limit butuh Redis-backed counter — di luar scope fase 9.
- **Dockerfile `context: .`** (NEW fase 9): pitfall `mokibox-go-shared` SKILL.md section
  "Docker build context untuk multi-package Go module" menjelaskan kenapa. Kalau ada service
  baru di compose yang juga butuh `go.mod`/`go.sum` dari root, ULANGI pattern `context: .`.
- **`golang.org/x/time` direct dependency** (NEW fase 9): dipromote dari indirect di fase 9.
  Tetap di track sebagai "production runtime dep", bukan "test-only" — kalau di-unrequire
  di fase 10, rate limit akan break.
- **`go-playground/validator/v10` direct dependency** (NEW fase 9.7): tambah via `go get`,
  promote via `go mod tidy`. Tag yang dipakai: required, omitempty, min, max, uuid, url.
  Refactor 4 call site: uploadIntentRequest, confirmRequest, updateMeRequest,
  commentRequestBody. Validator translation di `api-gateway/request_validator.go` —
  kalau fase 10 tambah tag baru atau localization, edit di sana.
- **Pre-commit verification** (Aturan #11): expected file list per issue ditulis sebelum
  `git add`, dicocokkan dengan `git status --porcelain`, dan `git log --stat -1` diverifikasi
  setelah commit. Fase 9 mengikuti pattern ini: 9 commit, semua expected files match.
- **PR body post-submit edit** (carry-over): `gh api -X PATCH .../pulls/<num> --input <json>`
  reliable; `gh pr edit --body-file` deprecated + exit 1 silent. Fase 9 pakai pattern ini
  2x (initial submit + followup update).
- **PR body angka self-check** (per git-phase-workflow rule #10): angka breakdown touchpoint
  di PR body diverifikasi dengan `git diff --shortstat main..HEAD`. Fase 9 PR body section
  "Commits" table juga catat perbedaan `git log --stat` per-commit sum (1382/219) vs
  `git diff --stat` (1357/194) dengan jelas, supaya reviewer tidak bingung.
- **Live HTTP smoke recipe** (NEW fase 9): butuh `make up-zitadel` dulu + cert populate
  di `deploy/nginx/certs/` + binary boot dengan env lengkap. Issue #30 fase 10 menulis
  integration smoke script yang ngebukti /healthz 200 + /api/videos/.../404 envelope
  + rate limit 429 + validator 400 envelope — semua skenario harus jalan end-to-end
  dengan Zitadel up.

## Files touched fase 9 (for future reference)

```
api-gateway/Dockerfile              (+97 / -13)
api-gateway/error_handler.go        (+185 / -0, new)
api-gateway/main.go                 (+221 / -102)  // A + E
api-gateway/middleware/ratelimit.go (+290 / -0, new)
api-gateway/routes.go               (+47 / -0)
docker-compose.yml                  (+12 / -2)
go.mod                              (+2 / -2)  // promote golang.org/x/time
shared/response.go                  (+17 / -1)  // classifyError -> ClassifyError
```

## Handoff prompt

`prompts/PHASE-10-PROMPT.md` (untracked) akan ditulis setelah fase 9 PR #45 merged,
sebelum fase 10 agent mulai kerja. Pattern carry-over dari fase 9 → fase 10 yang
WAJIB di-promote ke prompt berikutnya:
- Verification gap on live HTTP smoke (binary fail-fast tanpa Zitadel infra)
- Distroless image no-shell caveat
- Per-process rate limit (bukan global) — kalau production scale, butuh Redis-backed
- Single-pool `*sql.DB` + `sql.ErrNoRows` only (carry dari fase 7-9)
- Counter race (carry dari fase 5-9)
- Orphan R2 reconciliation (issue #44)